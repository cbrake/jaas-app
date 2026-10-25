package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	errChunkGap           = errors.New("chunk out of order")
	errChunkForbidden     = errors.New("stream belongs to another participant")
	errChunkClosed        = errors.New("stream already closed")
	errChunkTooLarge      = errors.New("stream exceeds size limit")
	errChunkConflict      = errors.New("chunk content does not match committed content")
	errAudioQuota         = errors.New("audio storage quota reached")
	ErrCaptureActive      = errors.New("capture still has active streams")
	ErrCaptureCancelled   = errors.New("capture cancelled")
	ErrCaptureNotApproved = errors.New("capture not approved for processing")
	ErrCaptureIncomplete  = errors.New("capture is missing a verified final manifest")
)

// CaptureHints are untrusted browser observations. They describe the capture;
// they do not authorize processing or prove acoustic quality/identity.
type CaptureHints struct {
	Language         string
	SharedMicrophone bool
	EndedAt          time.Time
	MetadataJSON     string
}

type captureRun struct {
	ID               int64
	RoomSlug         string
	Started          time.Time
	LastSeen         time.Time
	Ended            time.Time
	Status           string
	ApprovedAt       time.Time
	CancelledAt      time.Time
	Language         string
	SharedMicrophone bool
	ClientName       string
	ProjectName      string
	OrganizerEmail   string
	Error            string
	StreamCount      int
	FailedStreams    int
	AudioBytes       int64
}

type AIUsage struct {
	OperationKey   string
	CaptureID      *int64
	MinutesID      *int64
	Stage          string
	Attempt        int
	ModelRequested string
	ModelReported  string
	RequestID      string
	Status         string
	UsageJSON      json.RawMessage
}

// Only provider counters go into UsageJSON. No prompt or transcript belongs
// here; unknown cost remains SQL NULL until independently reconciled.
func (db *DB) RecordAIUsage(u AIUsage) error {
	if len(u.UsageJSON) > 65536 || (len(u.UsageJSON) > 0 && !json.Valid(u.UsageJSON)) {
		return errors.New("invalid usage metadata")
	}
	var usage any
	if len(u.UsageJSON) > 0 {
		usage = string(u.UsageJSON)
	}
	_, err := db.conn.Exec(`INSERT INTO ai_usage(operation_key,capture_id,minutes_id,stage,attempt,model_requested,model_reported,request_id,status,usage_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(operation_key) DO UPDATE SET model_reported=excluded.model_reported,request_id=excluded.request_id,status=excluded.status,usage_json=excluded.usage_json`, u.OperationKey, u.CaptureID, u.MinutesID, u.Stage, u.Attempt, u.ModelRequested, u.ModelReported, u.RequestID, u.Status, usage, db.unix())
	return err
}

const captureColumns = `r.id,r.room_slug,r.started_at,r.last_activity_at,r.ended_at,r.status,
	r.processing_approved_at,r.cancelled_at,r.language,r.shared_microphone,r.client_name,r.project_name,r.organizer_email,COALESCE(r.error,''),
	(SELECT COUNT(*) FROM audio_streams s WHERE s.run_id=r.id),
	(SELECT COUNT(*) FROM audio_streams s WHERE s.run_id=r.id AND s.status='failed'),
	(SELECT COALESCE(SUM(bytes),0) FROM audio_streams s WHERE s.run_id=r.id)`

func scanCapture(row interface{ Scan(...any) error }) (*captureRun, error) {
	r := &captureRun{}
	var start, last int64
	var end, approved, cancelled sql.NullInt64
	err := row.Scan(&r.ID, &r.RoomSlug, &start, &last, &end, &r.Status, &approved, &cancelled, &r.Language, &r.SharedMicrophone, &r.ClientName, &r.ProjectName, &r.OrganizerEmail, &r.Error, &r.StreamCount, &r.FailedStreams, &r.AudioBytes)
	r.Started = time.Unix(start, 0).UTC()
	r.LastSeen = time.Unix(last, 0).UTC()
	r.Ended = fromUnix(end)
	r.ApprovedAt = fromUnix(approved)
	r.CancelledAt = fromUnix(cancelled)
	return r, err
}

func (db *DB) GetCaptureRun(id int64) (*captureRun, error) {
	r, err := scanCapture(db.conn.QueryRow("SELECT "+captureColumns+" FROM capture_runs r WHERE r.id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// An empty owner is for a caller already authorized as superadmin. Handlers
// must obtain the owner from the verified session, never from a query string.
func (db *DB) ListCaptureRuns(owner string, limit, offset int) ([]captureRun, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	q := "SELECT " + captureColumns + " FROM capture_runs r JOIN rooms ro ON ro.slug=r.room_slug"
	args := []any{}
	if owner != "" {
		q += " WHERE ro.owner_email=?"
		args = append(args, owner)
	}
	q += " ORDER BY r.started_at DESC,r.id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []captureRun{}
	for rows.Next() {
		r, err := scanCapture(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (db *DB) UpdateCaptureDetails(id int64, client, project, organizer string) error {
	client = strings.TrimSpace(client)
	project = strings.TrimSpace(project)
	organizer = strings.TrimSpace(organizer)
	if utf8.RuneCountInString(client) > 120 || utf8.RuneCountInString(project) > 120 || len(organizer) > 254 {
		return errors.New("capture details too long")
	}
	if organizer != "" && normalizeEmail(organizer) == "" {
		return errors.New("invalid organizer email")
	}
	res, err := db.conn.Exec(`UPDATE capture_runs SET client_name=?,project_name=?,organizer_email=? WHERE id=?`, client, project, organizer, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func (db *DB) ApproveCapture(id int64) error {
	db.captureMu.Lock()
	defer db.captureMu.Unlock()
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cancelled, approved sql.NullInt64
	var status string
	if err = tx.QueryRow("SELECT status,cancelled_at,processing_approved_at FROM capture_runs WHERE id=?", id).Scan(&status, &cancelled, &approved); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if cancelled.Valid {
		return ErrCaptureCancelled
	}
	if approved.Valid {
		return nil
	}
	if status != "open" && status != "transcribing" {
		return errors.New("capture is not awaiting approval")
	}
	now := db.now()
	var active, expired, incomplete int
	if err = tx.QueryRow("SELECT COUNT(*) FROM audio_streams WHERE run_id=? AND status='recording' AND last_chunk_at>=?", id, now.Add(-streamIdleClose).Unix()).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrCaptureActive
	}
	if err = tx.QueryRow("SELECT COUNT(*) FROM audio_streams WHERE run_id=? AND finalized_at IS NULL", id).Scan(&incomplete); err != nil {
		return err
	}
	if incomplete > 0 {
		return ErrCaptureIncomplete
	}
	if err = tx.QueryRow("SELECT COUNT(*) FROM audio_streams WHERE run_id=? AND (purged_at IS NOT NULL OR status='failed')", id).Scan(&expired); err != nil {
		return err
	}
	if expired > 0 {
		return errors.New("capture contains unavailable or failed audio; review required")
	}
	if _, err = tx.Exec("UPDATE audio_streams SET status='closed' WHERE run_id=? AND status='recording'", id); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE capture_runs SET processing_approved_at=?,status='transcribing',error=NULL WHERE id=?", now.Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) CancelCapture(id int64) error {
	db.captureMu.Lock()
	defer db.captureMu.Unlock()
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slug string
	if err = tx.QueryRow("SELECT room_slug FROM capture_runs WHERE id=?", id).Scan(&slug); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	now := db.unix()
	session := "captura-" + strconv.FormatInt(id, 10)
	if _, err = tx.Exec("UPDATE capture_runs SET cancelled_at=COALESCE(cancelled_at,?) WHERE id=?", now, id); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE audio_streams SET status='closed' WHERE run_id=? AND status='recording'", id); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE minutes SET cancelled_at=COALESCE(cancelled_at,?),updated_at=? WHERE room_slug=? AND session_id=?", now, now, slug, session); err != nil {
		return err
	}
	for _, table := range []string{"minutes_deliveries", "host_notifications"} {
		if _, err = tx.Exec("UPDATE "+table+" SET status='failed',error='cancelada por anfitrión' WHERE status='queued' AND minutes_id IN (SELECT id FROM minutes WHERE room_slug=? AND session_id=?)", slug, session); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if cancel := db.captureCancels[id]; cancel != nil {
		cancel()
	}
	return nil
}

func validCaptureLanguage(language string) bool {
	return language == "es" || language == "en" || language == "mixed"
}

func normalizeCaptureHints(h CaptureHints) (CaptureHints, error) {
	if h.Language == "" {
		h.Language = "es"
	}
	if !validCaptureLanguage(h.Language) {
		return h, errors.New("unsupported capture language")
	}
	if h.MetadataJSON == "" {
		h.MetadataJSON = "{}"
	}
	if len(h.MetadataJSON) > 4096 {
		return h, errors.New("capture metadata too large")
	}
	var source map[string]json.RawMessage
	if err := json.Unmarshal([]byte(h.MetadataJSON), &source); err != nil || source == nil {
		return h, errors.New("invalid capture metadata")
	}
	clean := map[string]any{}
	for _, key := range []string{"sample_rate", "channels", "bitrate"} {
		if raw, ok := source[key]; ok {
			var n int64
			if json.Unmarshal(raw, &n) != nil || n < 0 || n > 10000000 {
				return h, errors.New("invalid capture settings")
			}
			clean[key] = n
		}
	}
	for _, key := range []string{"mime", "device_label"} {
		if raw, ok := source[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) != nil || utf8.RuneCountInString(s) > 160 {
				return h, errors.New("invalid capture settings")
			}
			clean[key] = s
		}
	}
	if raw, ok := source["speech_detected"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return h, errors.New("invalid capture settings")
		}
		clean["speech_detected"] = b
	}
	if raw, ok := source["signal_test"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			clean["signal_test"] = b
		} else {
			var result string
			if json.Unmarshal(raw, &result) != nil {
				return h, errors.New("invalid capture settings")
			}
			switch result {
			case "not_run", "signal_detected", "low_signal", "clipping", "unavailable":
				clean["signal_test"] = result
			default:
				return h, errors.New("invalid signal test result")
			}
		}
	}
	b, _ := json.Marshal(clean)
	h.MetadataJSON = string(b)
	return h, nil
}

func (app *App) AppendChunk(p *ParticipantSession, id string, seq int, start time.Time, data []byte) error {
	return app.AppendChunkWithMetadata(p, id, seq, start, data, CaptureHints{Language: "es"})
}

func (app *App) AppendChunkWithMetadata(p *ParticipantSession, id string, seq int, start time.Time, data []byte, hints CaptureHints) error {
	if p == nil || !streamIDPattern.MatchString(id) || seq < 0 {
		return errChunkForbidden
	}
	if len(data) == 0 || len(data) > chunkBodyLimit {
		return errChunkTooLarge
	}
	h, err := normalizeCaptureHints(hints)
	if err != nil {
		return err
	}
	db := app.DB
	db.captureMu.Lock()
	defer db.captureMu.Unlock()
	now := db.now()
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var slug, email, status, language string
	var shared bool
	var next int
	var committed, runID, started int64
	err = tx.QueryRow("SELECT room_slug,email,status,next_seq,bytes,run_id,started_at,language,shared_microphone FROM audio_streams WHERE id=?", id).Scan(&slug, &email, &status, &next, &committed, &runID, &started, &language, &shared)
	if errors.Is(err, sql.ErrNoRows) {
		if seq != 0 {
			return errChunkGap
		}
		runID, err = openRun(tx, p.RoomSlug, now)
		if err != nil {
			return err
		}
		if d := now.Sub(start); d < 0 || d > time.Minute {
			start = now
		}
		started = start.UnixMilli()
		_, err = tx.Exec(`INSERT INTO audio_streams(id,room_slug,run_id,email,display_name,started_at,last_chunk_at,next_seq,bytes,status,metadata_json,language,shared_microphone) VALUES(?,?,?,?,?,?,?,0,0,'recording',?,?,?)`, id, p.RoomSlug, runID, p.Email, p.DisplayName, started, now.Unix(), h.MetadataJSON, h.Language, h.SharedMicrophone)
		if err != nil {
			return err
		}
		slug = p.RoomSlug
		email = p.Email
		status = "recording"
		language = h.Language
		shared = h.SharedMicrophone
	} else if err != nil {
		return err
	}
	if slug != p.RoomSlug || email != p.Email {
		return errChunkForbidden
	}
	var approved, cancelled sql.NullInt64
	if err = tx.QueryRow("SELECT processing_approved_at,cancelled_at FROM capture_runs WHERE id=?", runID).Scan(&approved, &cancelled); err != nil {
		return err
	}
	if cancelled.Valid {
		return errChunkClosed
	}
	if language != h.Language || shared != h.SharedMicrophone {
		return errChunkConflict
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if seq < next {
		var saved string
		var offset, size int64
		if err = tx.QueryRow("SELECT sha256,offset_bytes,size_bytes FROM audio_chunks WHERE stream_id=? AND seq=?", id, seq).Scan(&saved, &offset, &size); err != nil {
			return errChunkConflict
		}
		if saved != digest || size != int64(len(data)) {
			return errChunkConflict
		}
		f, err := os.Open(app.streamPath(id))
		if err != nil {
			return errChunkConflict
		}
		defer f.Close()
		hash := sha256.New()
		if _, err = io.Copy(hash, io.NewSectionReader(f, offset, size)); err != nil || hex.EncodeToString(hash.Sum(nil)) != saved {
			return errChunkConflict
		}
		return nil
	}
	if status != "recording" || approved.Valid {
		return errChunkClosed
	}
	if seq > next {
		return errChunkGap
	}
	if committed+int64(len(data)) > streamMaxBytes {
		return errChunkTooLarge
	}
	if err = os.MkdirAll(app.audioDir(), 0o700); err != nil {
		return err
	}
	used, err := app.audioStorageBytes()
	if err != nil {
		return err
	}
	quota := app.Config.AudioMaxStorageBytes
	if quota == 0 {
		quota = 10 << 30
	}
	path := app.streamPath(id)
	if info, e := os.Lstat(path); e == nil && !info.Mode().IsRegular() {
		return errors.New("invalid audio file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < committed {
		return errors.New("committed audio is incomplete")
	}
	if used-(info.Size()-committed)+int64(len(data)) > quota {
		return errAudioQuota
	}
	// Recover an uncommitted append after a crash or failed SQLite commit.
	if err = f.Truncate(committed); err != nil {
		return err
	}
	if _, err = f.WriteAt(data, committed); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = syncCaptureDirectory(app.audioDir()); err != nil {
		return err
	}
	end := h.EndedAt
	if end.IsZero() || end.UnixMilli() < started || end.After(now.Add(5*time.Second)) {
		end = now
	}
	if _, err = tx.Exec("INSERT INTO audio_chunks(stream_id,seq,sha256,offset_bytes,size_bytes) VALUES(?,?,?,?,?)", id, seq, digest, committed, len(data)); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE audio_streams SET next_seq=?,bytes=?,last_chunk_at=?,ended_at_ms=MAX(COALESCE(ended_at_ms,0),?) WHERE id=?", seq+1, committed+int64(len(data)), now.Unix(), end.UnixMilli(), id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE capture_runs SET last_activity_at=?,shared_microphone=MAX(shared_microphone,?),language=CASE
		WHEN EXISTS(SELECT 1 FROM audio_streams WHERE run_id=? AND language='mixed') THEN 'mixed'
		WHEN (SELECT COUNT(DISTINCT language) FROM audio_streams WHERE run_id=?)>1 THEN 'mixed'
		ELSE (SELECT language FROM audio_streams WHERE run_id=? LIMIT 1) END WHERE id=?`, now.Unix(), h.SharedMicrophone, runID, runID, runID, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func openRun(tx *sql.Tx, slug string, now time.Time) (int64, error) {
	var id int64
	err := tx.QueryRow("SELECT id FROM capture_runs WHERE room_slug=? AND status='open' AND cancelled_at IS NULL AND processing_approved_at IS NULL AND last_activity_at>? ORDER BY id DESC LIMIT 1", slug, now.Add(-runMaxGap).Unix()).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := tx.Exec("INSERT INTO capture_runs(room_slug,started_at,last_activity_at,status,organizer_email) SELECT slug,?,?,'open',owner_email FROM rooms WHERE slug=?", now.Unix(), now.Unix(), slug)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrNotFound
	}
	return res.LastInsertId()
}

// FinalizeStream is an explicit end manifest, not an inactivity heuristic.
// The sender must acknowledge every queued chunk before supplying the total
// count and bytes; a missing final chunk cannot pass this boundary.
func (app *App) FinalizeStream(p *ParticipantSession, id string, nextSeq int, bytes int64, end time.Time) error {
	if p == nil || !streamIDPattern.MatchString(id) || nextSeq < 1 || bytes < 1 || bytes > streamMaxBytes {
		return errChunkForbidden
	}
	db := app.DB
	db.captureMu.Lock()
	defer db.captureMu.Unlock()
	var slug, email, status string
	var expectedSeq int
	var expectedBytes, startMS, runID int64
	var finalized, cancelled sql.NullInt64
	err := db.conn.QueryRow(`SELECT s.room_slug,s.email,s.status,s.next_seq,s.bytes,s.started_at,s.run_id,s.finalized_at,r.cancelled_at FROM audio_streams s JOIN capture_runs r ON r.id=s.run_id WHERE s.id=?`, id).Scan(&slug, &email, &status, &expectedSeq, &expectedBytes, &startMS, &runID, &finalized, &cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if slug != p.RoomSlug || email != p.Email {
		return errChunkForbidden
	}
	if cancelled.Valid {
		return ErrCaptureCancelled
	}
	if expectedSeq != nextSeq || expectedBytes != bytes {
		return ErrCaptureIncomplete
	}
	if finalized.Valid {
		return nil
	}
	if status != "recording" && status != "closed" {
		return errChunkClosed
	}
	audio, err := os.ReadFile(app.streamPath(id))
	if err != nil {
		return ErrCaptureIncomplete
	}
	if err = app.verifyCommittedAudio(id, audio); err != nil {
		return ErrCaptureIncomplete
	}
	now := db.now()
	if end.IsZero() || end.UnixMilli() < startMS || end.After(now.Add(5*time.Second)) {
		end = now
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE audio_streams SET status='closed',finalized_at=?,final_seq=?,final_bytes=?,ended_at_ms=MAX(COALESCE(ended_at_ms,0),?) WHERE id=?", now.Unix(), nextSeq, bytes, end.UnixMilli(), id); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE capture_runs SET ended_at=MAX(COALESCE(ended_at,0),?) WHERE id=?", end.Unix(), runID); err != nil {
		return err
	}
	return tx.Commit()
}

func syncCaptureDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	} // Windows does not support directory fsync.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (app *App) audioStorageBytes() (int64, error) {
	entries, err := os.ReadDir(app.audioDir())
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var bytes int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		if info.Mode().IsRegular() {
			bytes += info.Size()
		}
	}
	return bytes, nil
}

func (app *App) processCapture(ctx context.Context) bool {
	db := app.DB
	now := db.now()
	if _, err := db.conn.Exec("UPDATE audio_streams SET status='closed' WHERE status='recording' AND last_chunk_at<?", now.Add(-streamIdleClose).Unix()); err != nil {
		slog.Error("capture: idle close failed")
		return false
	}
	if app.Transcriber == nil {
		return false
	}
	var id string
	var attempts int
	err := db.conn.QueryRow(`SELECT s.id,s.attempts FROM audio_streams s JOIN capture_runs r ON r.id=s.run_id WHERE r.status='transcribing' AND r.processing_approved_at IS NOT NULL AND r.cancelled_at IS NULL AND s.status='closed' AND s.finalized_at IS NOT NULL AND s.purged_at IS NULL AND s.next_attempt_at<=? ORDER BY s.started_at,s.id LIMIT 1`, now.Unix()).Scan(&id, &attempts)
	if err == nil {
		app.transcribeStream(ctx, id, attempts+1)
		return true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		slog.Error("capture: query failed")
		return false
	}
	var runID int64
	err = db.conn.QueryRow(`SELECT r.id FROM capture_runs r WHERE r.status='transcribing' AND r.processing_approved_at IS NOT NULL AND r.cancelled_at IS NULL AND NOT EXISTS(SELECT 1 FROM audio_streams s WHERE s.run_id=r.id AND s.status IN ('recording','closed')) ORDER BY r.id LIMIT 1`).Scan(&runID)
	if err != nil {
		return false
	}
	run, err := db.GetCaptureRun(runID)
	if err != nil {
		return false
	}
	app.finishRun(run)
	return false // do not spin on a failed database consolidation
}

func (app *App) resultPath(id string) string  { return filepath.Join(app.audioDir(), id+".result.json") }
func (app *App) requestPath(id string) string { return filepath.Join(app.audioDir(), id+".request") }

func (app *App) verifyCommittedAudio(id string, audio []byte) error {
	var size int64
	if err := app.DB.conn.QueryRow("SELECT bytes FROM audio_streams WHERE id=?", id).Scan(&size); err != nil || size != int64(len(audio)) {
		return permanent("tamaño de audio no coincide con el registro; revisión manual")
	}
	rows, err := app.DB.conn.Query("SELECT seq,offset_bytes,size_bytes,sha256 FROM audio_chunks WHERE stream_id=? ORDER BY seq", id)
	if err != nil {
		return permanent("no se pudo verificar el registro de audio")
	}
	defer rows.Close()
	var offset int64
	seq := 0
	for rows.Next() {
		var actualSeq int
		var start, n int64
		var expected string
		if rows.Scan(&actualSeq, &start, &n, &expected) != nil || actualSeq != seq || start != offset || n <= 0 || n > size-offset {
			return permanent("registro de fragmentos incompleto; revisión manual")
		}
		sum := sha256.Sum256(audio[offset : offset+n])
		if hex.EncodeToString(sum[:]) != expected {
			return permanent("hash de audio no coincide; revisión manual")
		}
		offset += n
		seq++
	}
	if rows.Err() != nil || seq == 0 || offset != size {
		return permanent("audio sin inventario completo de fragmentos; revisión manual")
	}
	return nil
}

type cachedCaptureResult struct {
	Attempt  int                 `json:"attempt"`
	Segments []transcriptSegment `json:"segments"`
}

func writeCaptureResult(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".capture-result-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncCaptureDirectory(filepath.Dir(path))
}

func (app *App) transcribeStream(parent context.Context, id string, attempt int) {
	db := app.DB
	db.captureMu.Lock()
	var runID int64
	var language string
	err := db.conn.QueryRow(`SELECT s.run_id,s.language FROM audio_streams s JOIN capture_runs r ON r.id=s.run_id WHERE s.id=? AND s.status='closed' AND s.finalized_at IS NOT NULL AND r.processing_approved_at IS NOT NULL AND r.cancelled_at IS NULL AND r.status='transcribing'`, id).Scan(&runID, &language)
	if err != nil {
		db.captureMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	if db.captureCancels == nil {
		db.captureCancels = map[int64]context.CancelFunc{}
	}
	db.captureCancels[runID] = cancel
	_, err = db.conn.Exec("UPDATE audio_streams SET attempts=?,next_attempt_at=? WHERE id=?", attempt, db.now().Add(15*time.Minute).Unix(), id)
	db.captureMu.Unlock()
	defer func() { cancel(); db.captureMu.Lock(); delete(db.captureCancels, runID); db.captureMu.Unlock() }()
	if err != nil {
		return
	}
	langs := []string{language}
	if language == "mixed" {
		langs = []string{"es", "en"}
	}
	ctx = withTranscriptionLanguages(ctx, langs)
	var segs []transcriptSegment
	providerStarted, uncertain := false, false
	usageAttempt := attempt
	model := app.Config.TranscribeModel
	if model == "" {
		model = "gpt-transcribe"
	}
	u := AIUsage{CaptureID: &runID, Stage: "transcription", Attempt: attempt, ModelRequested: model, Status: "started"}
	u.OperationKey = fmt.Sprintf("transcription:%s:%d", id, attempt)
	cached, cacheErr := os.ReadFile(app.resultPath(id))
	if cacheErr == nil {
		var result cachedCaptureResult
		if json.Unmarshal(cached, &result) != nil || result.Attempt < 1 {
			err = permanent("resultado durable inválido; revisión manual")
		} else {
			segs = result.Segments
			providerStarted = true
			usageAttempt = result.Attempt
			u.Attempt = usageAttempt
			u.OperationKey = fmt.Sprintf("transcription:%s:%d", id, usageAttempt)
		}
	} else if !os.IsNotExist(cacheErr) {
		err = permanent("resultado durable no disponible; revisión manual")
	} else {
		if marker, e := os.ReadFile(app.requestPath(id)); e == nil {
			var previous struct {
				Attempt int `json:"attempt"`
			}
			if json.Unmarshal(marker, &previous) == nil && previous.Attempt > 0 {
				u.Attempt = previous.Attempt
				u.OperationKey = fmt.Sprintf("transcription:%s:%d", id, previous.Attempt)
			}
			providerStarted, uncertain = true, true
			err = permanent("solicitud anterior sin resultado durable; revisión manual")
		} else if !os.IsNotExist(e) {
			err = permanent("estado de solicitud no disponible; revisión manual")
		}
		var audio []byte
		if err == nil {
			audio, err = os.ReadFile(app.streamPath(id))
			if err != nil || len(audio) == 0 {
				err = permanent("archivo de audio no disponible o vacío")
			}
		}
		if err == nil {
			err = app.verifyCommittedAudio(id, audio)
		}
		if err == nil {
			if e := db.RecordAIUsage(u); e != nil {
				err = permanent("no se pudo registrar el intento; no se llamó al proveedor")
			}
		}
		if err == nil {
			// Persist a request marker before calling the provider. If the process
			// crashes after a possibly billable request, recovery fails closed.
			f, e := os.OpenFile(app.requestPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if e != nil {
				err = permanent("no se pudo registrar la solicitud")
			} else {
				marker, _ := json.Marshal(struct {
					Attempt   int   `json:"attempt"`
					CreatedAt int64 `json:"created_at"`
				}{attempt, db.unix()})
				_, e = f.Write(marker)
				if e == nil {
					e = f.Sync()
				}
				closeErr := f.Close()
				if e == nil {
					e = closeErr
				}
				if e == nil {
					e = syncCaptureDirectory(app.audioDir())
				}
				if e != nil {
					err = permanent("no se pudo confirmar la solicitud")
				}
			}
		}
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err == nil {
			providerStarted = true
			segs, err = app.Transcriber.Transcribe(ctx, audio, "pista.webm")
			if err == nil {
				b, e := json.Marshal(cachedCaptureResult{Attempt: attempt, Segments: segs})
				if e == nil {
					e = writeCaptureResult(app.resultPath(id), b)
				}
				if e != nil {
					uncertain = true
					err = permanent("resultado no persistido; audio conservado; revisión manual")
				}
			} else {
				var responseError *httpStatusError
				if errors.As(err, &responseError) {
					if e := os.Remove(app.requestPath(id)); e != nil {
						err = permanent("solicitud fallida sin cierre durable; revisión manual")
					}
				} else {
					uncertain = true
					err = permanent("resultado de solicitud incierto; revisión manual")
				}
			}
		}
	}
	u.Status = "failed"
	if !providerStarted {
		u.Status = "not_sent"
	} else if uncertain {
		u.Status = "uncertain"
	} else if err == nil {
		u.Status = "succeeded"
	}
	if len(segs) > 0 {
		u.RequestID = segs[0].RequestID
		u.ModelReported = segs[0].Model
		u.UsageJSON = segs[0].Usage
	}
	if e := db.RecordAIUsage(u); e != nil {
		slog.Error("capture: usage persistence failed")
	}
	db.captureMu.Lock()
	defer db.captureMu.Unlock()
	var cancelled sql.NullInt64
	if e := db.conn.QueryRow("SELECT cancelled_at FROM capture_runs WHERE id=?", runID).Scan(&cancelled); e != nil || cancelled.Valid {
		return
	}
	if err == nil {
		b, e := json.Marshal(segs)
		if e != nil {
			return
		}
		if _, e = db.conn.Exec("UPDATE audio_streams SET status='transcribed',transcript=?,error=NULL WHERE id=?", string(b), id); e != nil {
			slog.Error("capture: persistence failed; cached result and audio retained")
			return
		}
		slog.Info("capture: stream transcribed", "segments", len(segs))
		return
	}
	if isPermanent(err) || attempt >= app.Config.WorkerMaxAttempts {
		if _, e := db.conn.Exec("UPDATE audio_streams SET status='failed',error=? WHERE id=?", err.Error(), id); e != nil {
			slog.Error("capture: failure persistence failed")
		}
		return
	}
	if _, e := db.conn.Exec("UPDATE audio_streams SET error=?,next_attempt_at=? WHERE id=?", err.Error(), db.now().Add(retryDelay(attempt)).Unix(), id); e != nil {
		slog.Error("capture: retry persistence failed")
	}
}

// finishRun atomically saves the consolidated transcript, enqueues one
// minutes job and marks the run complete. Incomplete input never becomes a
// supposedly complete meeting; original per-stream evidence remains intact.
func (app *App) finishRun(run *captureRun) {
	db := app.DB
	db.captureMu.Lock()
	defer db.captureMu.Unlock()
	tx, err := db.conn.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	var approved, cancelled sql.NullInt64
	var status string
	if err = tx.QueryRow("SELECT status,processing_approved_at,cancelled_at FROM capture_runs WHERE id=?", run.ID).Scan(&status, &approved, &cancelled); err != nil || !approved.Valid || cancelled.Valid || status != "transcribing" {
		return
	}
	var failed int
	if err = tx.QueryRow("SELECT COUNT(*) FROM audio_streams WHERE run_id=? AND (status!='transcribed' OR finalized_at IS NULL)", run.ID).Scan(&failed); err != nil {
		return
	}
	if failed > 0 {
		tx.Exec("UPDATE capture_runs SET status='failed',error='captura parcial: revisar pistas fallidas antes de generar minuta' WHERE id=?", run.ID)
		tx.Commit()
		return
	}
	rows, err := tx.Query("SELECT display_name,started_at,COALESCE(ended_at_ms,last_chunk_at*1000),COALESCE(transcript,''),shared_microphone FROM audio_streams WHERE run_id=? ORDER BY started_at,id", run.ID)
	if err != nil {
		return
	}
	var lines []timedLine
	var audioEnd time.Time
	hasShared := false
	hasWindow := false
	for rows.Next() {
		var name, raw string
		var startMs, endMs int64
		var shared bool
		if err = rows.Scan(&name, &startMs, &endMs, &raw, &shared); err != nil {
			rows.Close()
			return
		}
		var segs []transcriptSegment
		if err = json.Unmarshal([]byte(raw), &segs); err != nil {
			rows.Close()
			return
		}
		base := time.UnixMilli(startMs)
		end := time.UnixMilli(endMs)
		if end.After(audioEnd) {
			audioEnd = end
		}
		if shared {
			name = "Micrófono compartido (hablante sin verificar)"
			hasShared = true
		}
		for _, s := range segs {
			if keepSegment(s) {
				var dur time.Duration
				at := base
				if s.TimePrecision == "window" {
					hasWindow = true
				} else {
					at = base.Add(time.Duration(s.Start * float64(time.Second)))
					if s.End > s.Start {
						dur = time.Duration((s.End - s.Start) * float64(time.Second))
					}
				}
				lines = append(lines, timedLine{At: at, Dur: dur, Name: name, Text: strings.TrimSpace(s.Text), Score: s.AvgLogprob, TimePrecision: s.TimePrecision})
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	// Similar-text de-duplication requires precise acoustic timestamps. Never
	// drop entire windows merely because different devices discuss one topic.
	if hasWindow {
		sortCaptureLines(lines)
	} else {
		lines = mergeTranscripts(lines)
	}
	if len(lines) == 0 {
		tx.Exec("UPDATE capture_runs SET status='failed',error='sin voz reconocible; revisión requerida' WHERE id=?", run.ID)
		tx.Commit()
		return
	}
	type message struct {
		Name            string `json:"name"`
		Content         string `json:"content"`
		Timestamp       int64  `json:"timestamp"`
		DurationMS      int64  `json:"duration_ms,omitempty"`
		TimePrecision   string `json:"time_precision,omitempty"`
		SpeakerVerified bool   `json:"speaker_verified"`
	}
	doc := struct {
		Source          string    `json:"source"`
		Messages        []message `json:"messages"`
		Complete        bool      `json:"capture_complete"`
		Shared          bool      `json:"shared_microphone"`
		TimePrecision   string    `json:"time_precision"`
		CoverageScope   string    `json:"coverage_scope"`
		MeetingCoverage string    `json:"meeting_coverage"`
	}{Source: "browser-capture", Complete: true, Shared: hasShared, TimePrecision: "segment", CoverageScope: "finalized_uploaded_streams", MeetingCoverage: "unverified"}
	if hasWindow {
		doc.TimePrecision = "window"
	}
	// A display name identifies a channel but is not human confirmation of who
	// was physically speaking, even when the channel is marked individual.
	for _, line := range lines {
		doc.Messages = append(doc.Messages, message{line.Name, line.Text, line.At.UnixMilli(), line.Dur.Milliseconds(), line.TimePrecision, false})
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return
	}
	session := "captura-" + strconv.FormatInt(run.ID, 10)
	now := db.unix()
	if _, err = tx.Exec(`INSERT INTO transcriptions(room_slug,session_id,data,created_at) VALUES(?,?,?,?) ON CONFLICT(room_slug,session_id) DO UPDATE SET data=excluded.data`, run.RoomSlug, session, string(data), now); err != nil {
		return
	}
	if _, err = tx.Exec(`INSERT INTO minutes(room_slug,session_id,status,next_attempt_at,meeting_at,created_at,updated_at) VALUES(?,?,'queued',?,?,?,?) ON CONFLICT(room_slug,session_id) DO NOTHING`, run.RoomSlug, session, now, run.Started.Unix(), now, now); err != nil {
		return
	}
	if audioEnd.Before(run.Started) {
		audioEnd = run.LastSeen
	}
	if _, err = tx.Exec("UPDATE capture_runs SET status='done',minutes_session_id=?,ended_at=? WHERE id=?", session, audioEnd.Unix(), run.ID); err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		slog.Error("capture: consolidation commit failed")
		return
	}
	app.wakeWorker()
}

func sortCaptureLines(lines []timedLine) {
	// Kept separate from mergeTranscripts to make window precision explicit.
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].At.Before(lines[j].At) })
}

func (app *App) purgeOrphanAudio() int {
	db := app.DB
	db.captureMu.Lock()
	defer db.captureMu.Unlock()
	days := app.Config.AudioRetentionDays
	if days == 0 {
		days = 30
	}
	cutoff := app.now().AddDate(0, 0, -days)
	entries, err := os.ReadDir(app.audioDir())
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".webm") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".webm")
		var runID int64
		var status string
		var last int64
		err = db.conn.QueryRow("SELECT run_id,status,last_chunk_at FROM audio_streams WHERE id=?", id).Scan(&runID, &status, &last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err == nil {
			if status == "recording" && time.Unix(last, 0).After(app.now().Add(-streamIdleClose)) {
				continue
			}
			if db.captureCancels[runID] != nil {
				continue
			}
			// Commit an explicit expiry before deleting; a pending run must not
			// later resume against an absent audio file.
			tx, e := db.conn.Begin()
			if e != nil {
				continue
			}
			_, e = tx.Exec("UPDATE audio_streams SET purged_at=?,status=CASE WHEN status IN ('recording','closed') THEN 'failed' ELSE status END,error=CASE WHEN status IN ('recording','closed') THEN 'audio vencido por retención' ELSE error END WHERE id=?", db.unix(), id)
			if e == nil {
				_, e = tx.Exec("UPDATE capture_runs SET status=CASE WHEN status IN ('open','transcribing') THEN 'failed' ELSE status END,error=CASE WHEN status IN ('open','transcribing') THEN 'audio vencido por retención; revisión requerida' ELSE error END WHERE id=?", runID)
			}
			if e != nil {
				tx.Rollback()
				continue
			}
			if e = tx.Commit(); e != nil {
				continue
			}
		}
		if os.Remove(filepath.Join(app.audioDir(), entry.Name())) == nil {
			removed++
			os.Remove(app.resultPath(id))
			os.Remove(app.requestPath(id))
		}
	}
	// Also remove expired sidecars after a room deletion or an interrupted
	// previous purge. They can contain transcript text and share the same TTL.
	for _, entry := range entries {
		name := entry.Name()
		id := ""
		if strings.HasSuffix(name, ".result.json") {
			id = strings.TrimSuffix(name, ".result.json")
		} else if strings.HasSuffix(name, ".request") {
			id = strings.TrimSuffix(name, ".request")
		} else if strings.HasPrefix(name, ".capture-result-") {
			id = "temporary"
		} else {
			continue
		}
		info, e := entry.Info()
		if e != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		var runID int64
		var purged sql.NullInt64
		e = db.conn.QueryRow("SELECT run_id,purged_at FROM audio_streams WHERE id=?", id).Scan(&runID, &purged)
		if e == nil && (!purged.Valid || db.captureCancels[runID] != nil) {
			continue
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			continue
		}
		os.Remove(filepath.Join(app.audioDir(), name))
	}
	return removed
}
