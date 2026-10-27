package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

// All timestamps are stored as INTEGER unix seconds (UTC). Mixing SQLite's
// CURRENT_TIMESTAMP strings with Go-formatted times makes comparisons
// unreliable, so the database never generates times itself.

type Room struct {
	ID            int64
	Slug          string
	HostHash      string
	Active        bool
	Transcription bool
	OwnerEmail    string
	ActivatedAt   time.Time
	CreatedAt     time.Time
}

type DB struct {
	conn *sql.DB
	now  func() time.Time
	// The service has one worker. This lock serializes capture state and file
	// commits in this process; database leases still protect durable retries.
	captureMu      sync.Mutex
	captureCancels map[int64]context.CancelFunc
}

var migrations = []string{
	`CREATE TABLE rooms (
		id            INTEGER PRIMARY KEY,
		slug          TEXT UNIQUE NOT NULL,
		host_hash     TEXT NOT NULL,
		active        INTEGER NOT NULL DEFAULT 0,
		transcription INTEGER NOT NULL DEFAULT 0,
		activated_at  INTEGER,
		created_at    INTEGER NOT NULL
	);
	CREATE TABLE recordings (
		id           INTEGER PRIMARY KEY,
		room_slug    TEXT NOT NULL,
		session_id   TEXT NOT NULL DEFAULT '',
		download_url TEXT NOT NULL,
		expires_at   INTEGER NOT NULL,
		duration_sec INTEGER NOT NULL DEFAULT 0,
		created_at   INTEGER NOT NULL
	);
	CREATE INDEX recordings_room ON recordings(room_slug);
	CREATE TABLE transcriptions (
		id         INTEGER PRIMARY KEY,
		room_slug  TEXT NOT NULL,
		session_id TEXT NOT NULL,
		data       TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE (room_slug, session_id)
	);
	CREATE TABLE minutes (
		id                INTEGER PRIMARY KEY,
		room_slug         TEXT NOT NULL,
		session_id        TEXT NOT NULL,
		status            TEXT NOT NULL CHECK (status IN ('queued','processing','ready','failed')),
		attempts          INTEGER NOT NULL DEFAULT 0,
		next_attempt_at   INTEGER NOT NULL,
		locked_until      INTEGER,
		source_url        TEXT,
		source_expires_at INTEGER,
		meeting_at        INTEGER NOT NULL,
		result_json       TEXT,
		model             TEXT,
		error             TEXT,
		created_at        INTEGER NOT NULL,
		updated_at        INTEGER NOT NULL,
		UNIQUE (room_slug, session_id)
	);
	CREATE INDEX minutes_pending ON minutes(status, next_attempt_at);
	CREATE TABLE webhook_events (
		idempotency_key TEXT PRIMARY KEY,
		event_type      TEXT NOT NULL,
		room_slug       TEXT,
		session_id      TEXT,
		received_at     INTEGER NOT NULL
	);
	CREATE TABLE admin_credentials (
		id            INTEGER PRIMARY KEY CHECK (id = 1),
		password_hash TEXT NOT NULL,
		must_change   INTEGER NOT NULL DEFAULT 1,
		updated_at    INTEGER NOT NULL
	);
	CREATE TABLE admin_sessions (
		token_hash   TEXT PRIMARY KEY,
		csrf_token   TEXT NOT NULL,
		created_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL
	);
	CREATE TABLE participant_sessions (
		token_hash   TEXT PRIMARY KEY,
		room_slug    TEXT NOT NULL,
		display_name TEXT NOT NULL,
		moderator    INTEGER NOT NULL,
		csrf_token   TEXT NOT NULL,
		created_at   INTEGER NOT NULL,
		expires_at   INTEGER NOT NULL
	);
	CREATE INDEX participant_sessions_room ON participant_sessions(room_slug);`,
	// v2: per-user rooms. Identity comes from Cloudflare Access; rooms with an
	// empty owner (created in password mode) are visible to admins only.
	`ALTER TABLE rooms ADD COLUMN owner_email TEXT NOT NULL DEFAULT '';
	CREATE INDEX rooms_owner ON rooms(owner_email);
	ALTER TABLE admin_sessions ADD COLUMN email TEXT NOT NULL DEFAULT '';`,
	// v3: participant e-mails (asked at join, echoed back by JaaS in
	// PARTICIPANT_JOINED) and host-approved deliveries of minutes.
	`ALTER TABLE participant_sessions ADD COLUMN email TEXT NOT NULL DEFAULT '';
	CREATE TABLE session_participants (
		id           INTEGER PRIMARY KEY,
		room_slug    TEXT NOT NULL,
		session_id   TEXT NOT NULL DEFAULT '',
		email        TEXT NOT NULL,
		display_name TEXT NOT NULL,
		moderator    INTEGER NOT NULL DEFAULT 0,
		source       TEXT NOT NULL CHECK (source IN ('join','jaas')),
		joined_at    INTEGER NOT NULL
	);
	CREATE INDEX session_participants_room ON session_participants(room_slug, joined_at);
	CREATE UNIQUE INDEX session_participants_jaas ON session_participants(room_slug, session_id, email) WHERE source = 'jaas';
	CREATE TABLE minutes_deliveries (
		id              INTEGER PRIMARY KEY,
		minutes_id      INTEGER NOT NULL REFERENCES minutes(id) ON DELETE CASCADE,
		email           TEXT NOT NULL,
		display_name    TEXT NOT NULL DEFAULT '',
		status          TEXT NOT NULL CHECK (status IN ('queued','sent','failed')),
		attempts        INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL,
		approved_by     TEXT NOT NULL,
		error           TEXT,
		created_at      INTEGER NOT NULL,
		sent_at         INTEGER,
		UNIQUE (minutes_id, email)
	);
	CREATE INDEX minutes_deliveries_due ON minutes_deliveries(status, next_attempt_at);`,
	// v4: "your minutes are ready" e-mail to the room owner (internal only).
	`CREATE TABLE host_notifications (
		minutes_id      INTEGER NOT NULL REFERENCES minutes(id) ON DELETE CASCADE,
		email           TEXT NOT NULL,
		status          TEXT NOT NULL CHECK (status IN ('queued','sent','failed')),
		attempts        INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL,
		error           TEXT,
		sent_at         INTEGER,
		PRIMARY KEY (minutes_id, email)
	);
	CREATE INDEX host_notifications_due ON host_notifications(status, next_attempt_at);`,
	// v5: browser capture. Each participant's page uploads its own microphone
	// (stream); the streams of one meeting form a run.
	`CREATE TABLE capture_runs (
		id                 INTEGER PRIMARY KEY,
		room_slug          TEXT NOT NULL,
		started_at         INTEGER NOT NULL,
		last_activity_at   INTEGER NOT NULL,
		ended_at           INTEGER,
		status             TEXT NOT NULL CHECK (status IN ('open','transcribing','done','failed')),
		minutes_session_id TEXT,
		error              TEXT
	);
	CREATE INDEX capture_runs_room ON capture_runs(room_slug, status);
	CREATE TABLE audio_streams (
		id              TEXT PRIMARY KEY,
		room_slug       TEXT NOT NULL,
		run_id          INTEGER NOT NULL REFERENCES capture_runs(id) ON DELETE CASCADE,
		email           TEXT NOT NULL,
		display_name    TEXT NOT NULL,
		started_at      INTEGER NOT NULL,
		last_chunk_at   INTEGER NOT NULL,
		next_seq        INTEGER NOT NULL,
		bytes           INTEGER NOT NULL,
		status          TEXT NOT NULL CHECK (status IN ('recording','closed','transcribed','failed')),
		attempts        INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL DEFAULT 0,
		transcript      TEXT,
		error           TEXT
	);
	CREATE INDEX audio_streams_run ON audio_streams(run_id, status);`,
	// v6: processing is an explicit host decision. Existing CHECK constraints
	// remain intact; cancellation and approval are independent durable facts.
	`ALTER TABLE capture_runs ADD COLUMN processing_approved_at INTEGER;
	ALTER TABLE capture_runs ADD COLUMN cancelled_at INTEGER;
	ALTER TABLE capture_runs ADD COLUMN language TEXT NOT NULL DEFAULT 'es';
	ALTER TABLE capture_runs ADD COLUMN shared_microphone INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE capture_runs ADD COLUMN client_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE capture_runs ADD COLUMN project_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE capture_runs ADD COLUMN organizer_email TEXT NOT NULL DEFAULT '';
	ALTER TABLE audio_streams ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '{}';
	ALTER TABLE audio_streams ADD COLUMN ended_at_ms INTEGER;
	ALTER TABLE audio_streams ADD COLUMN finalized_at INTEGER;
	ALTER TABLE audio_streams ADD COLUMN final_seq INTEGER;
	ALTER TABLE audio_streams ADD COLUMN final_bytes INTEGER;
	ALTER TABLE audio_streams ADD COLUMN shared_microphone INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE audio_streams ADD COLUMN language TEXT NOT NULL DEFAULT 'es';
	ALTER TABLE audio_streams ADD COLUMN purged_at INTEGER;
	ALTER TABLE minutes ADD COLUMN cancelled_at INTEGER;
	CREATE TABLE audio_chunks (
		stream_id TEXT NOT NULL REFERENCES audio_streams(id) ON DELETE CASCADE,
		seq INTEGER NOT NULL,
		sha256 TEXT NOT NULL,
		offset_bytes INTEGER NOT NULL,
		size_bytes INTEGER NOT NULL,
		PRIMARY KEY (stream_id, seq)
	);
	CREATE INDEX capture_approval ON capture_runs(cancelled_at, processing_approved_at, status);
	CREATE TABLE ai_usage (
		id INTEGER PRIMARY KEY,
		operation_key TEXT UNIQUE NOT NULL,
		capture_id INTEGER,
		minutes_id INTEGER,
		stage TEXT NOT NULL,
		attempt INTEGER NOT NULL,
		model_requested TEXT NOT NULL,
		model_reported TEXT,
		request_id TEXT,
		status TEXT NOT NULL,
		usage_json TEXT,
		created_at INTEGER NOT NULL,
		estimated_cost REAL
	);`,
	// v7: durable Odoo events. There is deliberately no historical backfill.
	odooSyncMigration,
	// v8: seal the owner correspondence without rewriting v7 events/history.
	`ALTER TABLE odoo_sync_identity ADD COLUMN owner_map_digest TEXT NOT NULL DEFAULT '';`,
}

func InitDB(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(4)
	db := &DB{conn: conn, now: time.Now}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate() error {
	var version int
	if err := db.conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.conn.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) Close() error { return db.conn.Close() }

func (db *DB) Ping(ctx context.Context) error { return db.conn.PingContext(ctx) }

func (db *DB) unix() int64 { return db.now().Unix() }

func fromUnix(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0).UTC()
}

// --- rooms ---

func (db *DB) CreateRoom(slug, hostHash, ownerEmail string) error {
	_, err := db.conn.Exec(
		"INSERT INTO rooms (slug, host_hash, owner_email, created_at) VALUES (?, ?, ?, ?)",
		slug, hostHash, ownerEmail, db.unix(),
	)
	return err
}

const roomColumns = "id, slug, host_hash, active, transcription, owner_email, activated_at, created_at"

func scanRoom(sc interface{ Scan(...any) error }) (*Room, error) {
	var r Room
	var activated, created sql.NullInt64
	if err := sc.Scan(&r.ID, &r.Slug, &r.HostHash, &r.Active, &r.Transcription, &r.OwnerEmail, &activated, &created); err != nil {
		return nil, err
	}
	r.ActivatedAt = fromUnix(activated)
	r.CreatedAt = fromUnix(created)
	return &r, nil
}

func (db *DB) GetRoom(slug string) (*Room, error) {
	r, err := scanRoom(db.conn.QueryRow("SELECT "+roomColumns+" FROM rooms WHERE slug = ?", slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func (db *DB) ListRooms() ([]Room, error) {
	return db.listRooms("SELECT " + roomColumns + " FROM rooms ORDER BY created_at DESC, id DESC")
}

func (db *DB) ListRoomsOwnedBy(email string) ([]Room, error) {
	return db.listRooms("SELECT "+roomColumns+" FROM rooms WHERE owner_email = ? ORDER BY created_at DESC, id DESC", email)
}

func (db *DB) listRooms(q string, args ...any) ([]Room, error) {
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rooms []Room
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		rooms = append(rooms, *r)
	}
	return rooms, rows.Err()
}

// DeleteRoom removes the room and every piece of data tied to it:
// recordings, transcripts, minutes and participant sessions. Webhook
// idempotency keys are kept (so late retries are still recognised as
// duplicates) but stripped of the room and session they referred to.
func (db *DB) DeleteRoom(slug string) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM recordings WHERE room_slug = ?",
		"DELETE FROM transcriptions WHERE room_slug = ?",
		"DELETE FROM minutes WHERE room_slug = ?",
		"DELETE FROM participant_sessions WHERE room_slug = ?",
		"DELETE FROM session_participants WHERE room_slug = ?",
		"DELETE FROM capture_runs WHERE room_slug = ?",
		"UPDATE webhook_events SET room_slug = NULL, session_id = NULL WHERE room_slug = ?",
		"DELETE FROM rooms WHERE slug = ?",
	} {
		if _, err := tx.Exec(q, slug); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) SetTranscription(slug string, enabled bool) error {
	_, err := db.conn.Exec("UPDATE rooms SET transcription = ? WHERE slug = ?", enabled, slug)
	return err
}

func (db *DB) SetRoomActive(slug string, active bool) error {
	if active {
		_, err := db.conn.Exec("UPDATE rooms SET active = 1, activated_at = ? WHERE slug = ?", db.unix(), slug)
		return err
	}
	_, err := db.conn.Exec("UPDATE rooms SET active = 0, activated_at = NULL WHERE slug = ?", slug)
	return err
}

func (db *DB) DeactivateExpiredRooms(maxAge time.Duration) error {
	_, err := db.conn.Exec(
		"UPDATE rooms SET active = 0, activated_at = NULL WHERE active = 1 AND activated_at < ?",
		db.now().Add(-maxAge).Unix(),
	)
	return err
}

// --- recordings ---

type Recording struct {
	ID          int64
	RoomSlug    string
	DownloadURL string
	ExpiresAt   time.Time
	DurationSec int
	CreatedAt   time.Time
}

func (db *DB) AddRecording(roomSlug, sessionID, downloadURL string, expiresAt time.Time, durationSec int) error {
	_, err := db.conn.Exec(
		"INSERT INTO recordings (room_slug, session_id, download_url, expires_at, duration_sec, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		roomSlug, sessionID, downloadURL, expiresAt.Unix(), durationSec, db.unix(),
	)
	return err
}

func (db *DB) ListRecordings(roomSlug string) ([]Recording, error) {
	rows, err := db.conn.Query(
		"SELECT id, room_slug, download_url, expires_at, duration_sec, created_at FROM recordings WHERE room_slug = ? AND expires_at > ? ORDER BY created_at DESC",
		roomSlug, db.unix(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var recs []Recording
	for rows.Next() {
		var r Recording
		var exp, created sql.NullInt64
		if err := rows.Scan(&r.ID, &r.RoomSlug, &r.DownloadURL, &exp, &r.DurationSec, &created); err != nil {
			return nil, err
		}
		r.ExpiresAt, r.CreatedAt = fromUnix(exp), fromUnix(created)
		recs = append(recs, r)
	}
	return recs, rows.Err()
}

// --- transcriptions ---

type Transcription struct {
	ID        int64
	RoomSlug  string
	SessionID string
	Data      string
	CreatedAt time.Time
}

// SaveTranscription stores the transcript once per (room, session).
func (db *DB) SaveTranscription(roomSlug, sessionID, data string) error {
	_, err := db.conn.Exec(
		"INSERT INTO transcriptions (room_slug, session_id, data, created_at) VALUES (?, ?, ?, ?) ON CONFLICT (room_slug, session_id) DO NOTHING",
		roomSlug, sessionID, data, db.unix(),
	)
	return err
}

func (db *DB) GetTranscriptionBySession(roomSlug, sessionID string) (*Transcription, error) {
	return db.getTranscription("room_slug = ? AND session_id = ?", roomSlug, sessionID)
}

func (db *DB) GetTranscription(id int64) (*Transcription, error) {
	return db.getTranscription("id = ?", id)
}

func (db *DB) getTranscription(where string, args ...any) (*Transcription, error) {
	var t Transcription
	var created sql.NullInt64
	err := db.conn.QueryRow("SELECT id, room_slug, session_id, data, created_at FROM transcriptions WHERE "+where, args...).
		Scan(&t.ID, &t.RoomSlug, &t.SessionID, &t.Data, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = fromUnix(created)
	return &t, nil
}

func (db *DB) ListTranscriptions(roomSlug string) ([]Transcription, error) {
	rows, err := db.conn.Query(
		"SELECT id, room_slug, session_id, created_at FROM transcriptions WHERE room_slug = ? ORDER BY created_at DESC",
		roomSlug,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []Transcription
	for rows.Next() {
		var t Transcription
		var created sql.NullInt64
		if err := rows.Scan(&t.ID, &t.RoomSlug, &t.SessionID, &created); err != nil {
			return nil, err
		}
		t.CreatedAt = fromUnix(created)
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

// --- webhook idempotency ---

// RecordWebhookEvent returns false when the idempotency key was already seen.
func (db *DB) RecordWebhookEvent(key, eventType, roomSlug, sessionID string) (bool, error) {
	res, err := db.conn.Exec(
		"INSERT INTO webhook_events (idempotency_key, event_type, room_slug, session_id, received_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (idempotency_key) DO NOTHING",
		key, eventType, nullIfEmpty(roomSlug), nullIfEmpty(sessionID), db.unix(),
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (db *DB) ForgetWebhookEvent(key string) error {
	_, err := db.conn.Exec("DELETE FROM webhook_events WHERE idempotency_key = ?", key)
	return err
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
