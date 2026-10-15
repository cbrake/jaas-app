package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Browser capture: when a room has transcription enabled, each participant's
// page records their own microphone (silence while muted in the meeting) and
// uploads it in short chunks. Chunks of one page session form one "stream"
// (a single WebM/Opus file on disk). Streams of the same room that overlap in
// time form a "run" (one meeting). When the run goes quiet, every stream is
// transcribed with OpenAI, the results are merged by wall-clock time with the
// speaker's name, and the usual minutes pipeline takes over. Audio files are
// deleted once transcribed.

const (
	chunkBodyLimit   = 1 << 20  // one chunk (15 s of 16 kbps Opus is ~30 KB)
	streamMaxBytes   = 24 << 20 // OpenAI audio upload limit is 25 MB
	streamIdleClose  = 2 * time.Minute
	runIdleClose     = 5 * time.Minute
	runMaxGap        = 30 * time.Minute
	audioOrphanAfter = 24 * time.Hour
)

var streamIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

func (app *App) audioDir() string {
	return filepath.Join(filepath.Dir(app.Config.DBPath), "audio")
}

func (app *App) streamPath(id string) string {
	return filepath.Join(app.audioDir(), id+".webm")
}

// --- storage ---

type audioStream struct {
	ID         string
	RoomSlug   string
	RunID      int64
	Email      string
	Name       string
	StartedAt  time.Time
	Bytes      int64
	NextSeq    int
	Status     string
	Transcript string
}

// AppendChunk records chunk seq of a stream. The first chunk creates the
// stream (bound to the participant session that uploads it) and attaches it
// to the room's open run, or opens a new one. Re-sending an already stored
// seq is acknowledged without writing twice; a gap is refused so the file
// is never corrupted.
func (app *App) AppendChunk(p *ParticipantSession, streamID string, seq int, clientStart time.Time, data []byte) error {
	db := app.DB
	now := db.now()
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var s audioStream
	var started int64
	err = tx.QueryRow("SELECT id, room_slug, email, next_seq, bytes, status, started_at FROM audio_streams WHERE id = ?", streamID).
		Scan(&s.ID, &s.RoomSlug, &s.Email, &s.NextSeq, &s.Bytes, &s.Status, &started)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if seq != 0 {
			return errChunkGap
		}
		runID, err := openRun(tx, p.RoomSlug, now)
		if err != nil {
			return err
		}
		// The client clock only shifts the stream start by its offset; a
		// far-off client clock is replaced by the server time.
		start := clientStart
		if d := now.Sub(start); d < 0 || d > time.Minute {
			start = now
		}
		if _, err := tx.Exec(
			`INSERT INTO audio_streams (id, room_slug, run_id, email, display_name, started_at, last_chunk_at, next_seq, bytes, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 'recording')`,
			streamID, p.RoomSlug, runID, p.Email, p.DisplayName, start.UnixMilli(), now.Unix()); err != nil {
			return err
		}
		s = audioStream{ID: streamID, RoomSlug: p.RoomSlug, Email: p.Email, Status: "recording"}
	case err != nil:
		return err
	}
	if s.RoomSlug != p.RoomSlug || s.Email != p.Email {
		return errChunkForbidden
	}
	if s.Status != "recording" {
		return errChunkClosed
	}
	if seq < s.NextSeq {
		return nil // duplicate retry
	}
	if seq > s.NextSeq {
		return errChunkGap
	}
	if s.Bytes+int64(len(data)) > streamMaxBytes {
		return errChunkTooLarge
	}

	if err := os.MkdirAll(app.audioDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(app.streamPath(streamID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE audio_streams SET next_seq = ?, bytes = bytes + ?, last_chunk_at = ? WHERE id = ?",
		seq+1, len(data), now.Unix(), streamID); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE capture_runs SET last_activity_at = ? WHERE id = (SELECT run_id FROM audio_streams WHERE id = ?)",
		now.Unix(), streamID); err != nil {
		return err
	}
	return tx.Commit()
}

var (
	errChunkGap       = errors.New("chunk out of order")
	errChunkForbidden = errors.New("stream belongs to another participant")
	errChunkClosed    = errors.New("stream already closed")
	errChunkTooLarge  = errors.New("stream exceeds size limit")
)

func openRun(tx *sql.Tx, slug string, now time.Time) (int64, error) {
	var id int64
	err := tx.QueryRow("SELECT id FROM capture_runs WHERE room_slug = ? AND status = 'open' AND last_activity_at > ? ORDER BY id DESC LIMIT 1",
		slug, now.Add(-runMaxGap).Unix()).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := tx.Exec("INSERT INTO capture_runs (room_slug, started_at, last_activity_at, status) VALUES (?, ?, ?, 'open')",
		slug, now.Unix(), now.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// --- upload endpoint ---

// handleAudioChunk: POST /m/{slug}/audio?stream=ID&seq=N&start=MS with the raw
// chunk as body. Requires the participant session of this room, its CSRF
// token (X-CSRF-Token) and a room with transcription enabled.
func (app *App) handleAudioChunk(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	p, err := app.DB.GetParticipantSession(app.cookieValue(r, "jaas_part"))
	if err != nil || p.RoomSlug != slug || !tokensEqual(r.Header.Get("X-CSRF-Token"), p.CSRFToken) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	room, err := app.DB.GetRoom(slug)
	if err != nil || !room.Transcription {
		http.Error(w, "Transcription disabled", http.StatusConflict)
		return
	}
	q := r.URL.Query()
	streamID := q.Get("stream")
	seq, err1 := strconv.Atoi(q.Get("seq"))
	startMs, err2 := strconv.ParseInt(q.Get("start"), 10, 64)
	if !streamIDPattern.MatchString(streamID) || err1 != nil || err2 != nil || seq < 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "audio/webm") {
		http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
		return
	}
	if len(data) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch err := app.AppendChunk(p, streamID, seq, time.UnixMilli(startMs), data); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errChunkGap):
		http.Error(w, "Conflict", http.StatusConflict)
	case errors.Is(err, errChunkForbidden):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, errChunkClosed), errors.Is(err, errChunkTooLarge):
		http.Error(w, "Gone", http.StatusGone)
	default:
		app.serverError(w, "audio chunk", err)
	}
}

// --- OpenAI speech-to-text ---

type transcriptSegment struct {
	Start        float64 `json:"start"`
	End          float64 `json:"end"`
	Text         string  `json:"text"`
	AvgLogprob   float64 `json:"avg_logprob"`
	NoSpeechProb float64 `json:"no_speech_prob"`
}

type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, filename string) ([]transcriptSegment, error)
}

type openAITranscriber struct {
	BaseURL  string
	Key      string
	Model    string
	Prompt   string
	Language string // ISO-639-1; empty lets the model detect it
	HTTP     *http.Client
}

func (t *openAITranscriber) Transcribe(ctx context.Context, audio []byte, filename string) ([]transcriptSegment, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	h.Set("Content-Type", "audio/webm")
	fw, err := mw.CreatePart(h)
	if err != nil {
		return nil, err
	}
	fw.Write(audio)
	// Only whisper-1 returns per-segment timestamps (verbose_json); newer
	// models such as gpt-transcribe accept json/text only (measured
	// 2026-10-15), so their stream becomes a single block at its start.
	format := "json"
	if strings.HasPrefix(t.Model, "whisper") {
		format = "verbose_json"
	}
	for k, v := range map[string]string{
		"model": t.Model, "language": t.Language, "response_format": format, "temperature": "0", "prompt": t.Prompt,
	} {
		if v != "" {
			mw.WriteField(k, v)
		}
	}
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/audio/transcriptions", &body)
	if err != nil {
		return nil, errors.New("openai audio: invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+t.Key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return nil, errors.New("openai audio: connection failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, errors.New("openai audio: read failed")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{Op: "openai audio", Code: resp.StatusCode}
	}
	var out struct {
		Segments []transcriptSegment `json:"segments"`
		Text     string              `json:"text"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, errors.New("openai audio: unexpected response")
	}
	if len(out.Segments) == 0 && strings.TrimSpace(out.Text) != "" {
		out.Segments = []transcriptSegment{{Start: 0, Text: out.Text}}
	}
	return out.Segments, nil
}

// --- merging ---

// Whisper is known to "hear" stock phrases in silence (muted periods are sent
// as silence); drop segments that are likely not speech. The list covers the
// usual English and Spanish subtitle credits.
var hallucinations = []string{
	"thanks for watching", "thank you for watching", "please subscribe", "subtitles by",
	"gracias por ver", "suscríbete", "suscribete", "subtítulos realizados por", "subtitulos realizados por",
	"amara.org", "gracias por su atención", "¡gracias!", "gracias.",
}

func keepSegment(s transcriptSegment) bool {
	text := strings.TrimSpace(s.Text)
	if text == "" || s.NoSpeechProb > 0.6 || (s.AvgLogprob != 0 && s.AvgLogprob < -1.2) {
		return false
	}
	l := strings.ToLower(text)
	for _, h := range hallucinations {
		if l == h || (len(l) < 40 && strings.Contains(l, h)) {
			return false
		}
	}
	return true
}

type timedLine struct {
	At    time.Time
	Dur   time.Duration
	Name  string
	Text  string
	Score float64
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) > 2 {
			out[w] = true
		}
	}
	return out
}

func similar(a, b string) bool {
	wa, wb := words(a), words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return false
	}
	inter := 0
	for w := range wa {
		if wb[w] {
			inter++
		}
	}
	small := len(wa)
	if len(wb) < small {
		small = len(wb)
	}
	return float64(inter)/float64(small) >= 0.7
}

// mergeTranscripts orders all speakers' lines by wall-clock time and drops
// echoes: the same words picked up by another participant's microphone at
// nearly the same moment are kept only once, under the clearest recording.
func mergeTranscripts(lines []timedLine) []timedLine {
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].At.Before(lines[j].At) })
	drop := make([]bool, len(lines))
	for i := range lines {
		for j := i + 1; j < len(lines) && lines[j].At.Sub(lines[i].At) <= 4*time.Second; j++ {
			if lines[i].Name == lines[j].Name || drop[i] || drop[j] || !similar(lines[i].Text, lines[j].Text) {
				continue
			}
			if lines[i].Score >= lines[j].Score {
				drop[j] = true
			} else {
				drop[i] = true
			}
		}
	}
	out := lines[:0:0]
	for i, l := range lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return out
}

// --- worker ---

type captureRun struct {
	ID       int64
	RoomSlug string
	Started  time.Time
	LastSeen time.Time
}

// processCapture advances one run: close idle streams, close a quiet run,
// transcribe its streams one per call, and finally hand the merged transcript
// to the minutes queue.
func (app *App) processCapture(ctx context.Context) bool {
	if app.Transcriber == nil {
		return false
	}
	db := app.DB
	now := db.now()
	db.conn.Exec("UPDATE audio_streams SET status = 'closed' WHERE status = 'recording' AND last_chunk_at < ?", now.Add(-streamIdleClose).Unix())
	db.conn.Exec(`UPDATE capture_runs SET status = 'transcribing' WHERE status = 'open' AND last_activity_at < ?
		AND NOT EXISTS (SELECT 1 FROM audio_streams s WHERE s.run_id = capture_runs.id AND s.status = 'recording')`,
		now.Add(-runIdleClose).Unix())

	// One stream per call keeps each step short and resumable.
	var id string
	var attempts int
	err := db.conn.QueryRow(`SELECT s.id, s.attempts FROM audio_streams s JOIN capture_runs r ON r.id = s.run_id
		WHERE r.status = 'transcribing' AND s.status = 'closed' AND s.next_attempt_at <= ? ORDER BY s.started_at LIMIT 1`, now.Unix()).Scan(&id, &attempts)
	if err == nil {
		app.transcribeStream(ctx, id, attempts+1)
		return true
	}
	if !errors.Is(err, sql.ErrNoRows) {
		slog.Error("capture: query failed")
		return false
	}

	var run captureRun
	var started, last int64
	err = db.conn.QueryRow(`SELECT id, room_slug, started_at, last_activity_at FROM capture_runs r WHERE status = 'transcribing'
		AND NOT EXISTS (SELECT 1 FROM audio_streams s WHERE s.run_id = r.id AND s.status IN ('recording','closed')) ORDER BY id LIMIT 1`).
		Scan(&run.ID, &run.RoomSlug, &started, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		slog.Error("capture: run query failed")
		return false
	}
	run.Started, run.LastSeen = time.Unix(started, 0), time.Unix(last, 0)
	app.finishRun(&run)
	return true
}

func (app *App) transcribeStream(ctx context.Context, id string, attempt int) {
	db := app.DB
	db.conn.Exec("UPDATE audio_streams SET attempts = ?, next_attempt_at = ? WHERE id = ?", attempt, db.now().Add(15*time.Minute).Unix(), id)
	audio, err := os.ReadFile(app.streamPath(id))
	if err == nil && len(audio) == 0 {
		err = permanent("empty audio track")
	}
	var segs []transcriptSegment
	if err == nil {
		segs, err = app.Transcriber.Transcribe(ctx, audio, "track.webm")
	} else if !isPermanent(err) {
		err = permanent("audio file not available")
	}
	if err == nil {
		b, _ := json.Marshal(segs)
		db.conn.Exec("UPDATE audio_streams SET status = 'transcribed', transcript = ?, error = NULL WHERE id = ?", string(b), id)
		os.Remove(app.streamPath(id))
		slog.Info("capture: stream transcribed", "segments", len(segs))
		return
	}
	if isPermanent(err) || attempt >= app.Config.WorkerMaxAttempts {
		db.conn.Exec("UPDATE audio_streams SET status = 'failed', error = ? WHERE id = ?", err.Error(), id)
		os.Remove(app.streamPath(id))
		slog.Warn("capture: stream failed", "attempt", attempt, "error", err.Error())
		return
	}
	db.conn.Exec("UPDATE audio_streams SET error = ?, next_attempt_at = ? WHERE id = ?", err.Error(), db.now().Add(retryDelay(attempt)).Unix(), id)
	slog.Warn("capture: stream retry scheduled", "attempt", attempt, "error", err.Error())
}

// finishRun merges the transcribed streams of a run into one transcript in
// the same JSON shape JaaS uses and enqueues the minutes for it.
func (app *App) finishRun(run *captureRun) {
	db := app.DB
	rows, err := db.conn.Query("SELECT display_name, started_at, COALESCE(transcript, '') FROM audio_streams WHERE run_id = ? AND status = 'transcribed'", run.ID)
	if err != nil {
		return
	}
	var lines []timedLine
	for rows.Next() {
		var name, raw string
		var startedMs int64
		if err := rows.Scan(&name, &startedMs, &raw); err != nil {
			continue
		}
		var segs []transcriptSegment
		json.Unmarshal([]byte(raw), &segs)
		base := time.UnixMilli(startedMs)
		for _, s := range segs {
			if keepSegment(s) {
				var dur time.Duration
				if s.End > s.Start {
					dur = time.Duration((s.End - s.Start) * float64(time.Second))
				}
				lines = append(lines, timedLine{
					At: base.Add(time.Duration(s.Start * float64(time.Second))), Dur: dur, Name: name,
					Text: strings.TrimSpace(s.Text), Score: s.AvgLogprob,
				})
			}
		}
	}
	rows.Close()
	lines = mergeTranscripts(lines)

	session := "captura-" + strconv.FormatInt(run.ID, 10)
	if len(lines) == 0 {
		db.conn.Exec("UPDATE capture_runs SET status = 'failed', error = 'no recognisable speech' WHERE id = ?", run.ID)
		slog.Warn("capture: run without speech", "room", run.RoomSlug, "run", run.ID)
		return
	}
	type msg struct {
		Name       string `json:"name"`
		Content    string `json:"content"`
		Timestamp  int64  `json:"timestamp"`
		DurationMs int64  `json:"duration_ms,omitempty"`
	}
	doc := struct {
		Source   string `json:"source"`
		Messages []msg  `json:"messages"`
	}{Source: "browser-capture"}
	for _, l := range lines {
		doc.Messages = append(doc.Messages, msg{Name: l.Name, Content: l.Text, Timestamp: l.At.UnixMilli(), DurationMs: l.Dur.Milliseconds()})
	}
	data, _ := json.Marshal(doc)
	end := lines[len(lines)-1].At
	if err := db.SaveTranscription(run.RoomSlug, session, string(data)); err != nil {
		slog.Error("capture: save transcript failed")
		return
	}
	if _, err := db.EnqueueMinutes(run.RoomSlug, session, "", db.now(), end); err != nil {
		slog.Error("capture: enqueue minutes failed")
		return
	}
	db.conn.Exec("UPDATE capture_runs SET status = 'done', minutes_session_id = ?, ended_at = ? WHERE id = ?", session, end.Unix(), run.ID)
	db.conn.Exec("UPDATE audio_streams SET transcript = NULL WHERE run_id = ?", run.ID)
	slog.Info("capture: transcript ready", "room", run.RoomSlug, "run", run.ID, "lines", len(lines))
	app.wakeWorker()
}

// purgeOrphanAudio deletes audio files that outlived their purpose (failed
// runs, abandoned uploads): no audio stays on disk more than a day.
func (app *App) purgeOrphanAudio() int {
	entries, err := os.ReadDir(app.audioDir())
	if err != nil {
		return 0
	}
	n := 0
	cutoff := app.now().Add(-audioOrphanAfter)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(app.audioDir(), e.Name())) == nil {
			n++
		}
	}
	return n
}

// deleteRoomAudio removes the audio files of a room before its rows go.
func (app *App) deleteRoomAudio(slug string) {
	rows, err := app.DB.conn.Query("SELECT id FROM audio_streams WHERE room_slug = ?", slug)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		os.Remove(app.streamPath(id))
	}
}

// captureSpeakers: participants who uploaded audio for a capture session.
func (db *DB) captureSpeakers(session string) ([][2]string, error) {
	runID, ok := strings.CutPrefix(session, "captura-")
	if !ok {
		return nil, nil
	}
	rows, err := db.conn.Query("SELECT DISTINCT email, display_name FROM audio_streams WHERE run_id = ? AND email != ''", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var e, n string
		if err := rows.Scan(&e, &n); err != nil {
			return nil, err
		}
		out = append(out, [2]string{e, n})
	}
	return out, rows.Err()
}
