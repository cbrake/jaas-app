package main

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Room struct {
	ID            int64
	Slug          string
	HostHash      string
	Active        bool
	Transcription bool
	CreatedAt     time.Time
}

type DB struct {
	conn *sql.DB
}

func InitDB(dsn string) (*DB, error) {
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	_, err = conn.Exec(`
		CREATE TABLE IF NOT EXISTS rooms (
			id         INTEGER PRIMARY KEY,
			slug       TEXT UNIQUE NOT NULL,
			host_hash  TEXT NOT NULL,
			active     BOOLEAN DEFAULT FALSE,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_, err = conn.Exec(`
		CREATE TABLE IF NOT EXISTS recordings (
			id           INTEGER PRIMARY KEY,
			room_slug    TEXT NOT NULL,
			kind         TEXT NOT NULL DEFAULT 'recording',
			download_url TEXT NOT NULL,
			expires_at   DATETIME NOT NULL,
			duration_sec INTEGER DEFAULT 0,
			created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		conn.Close()
		return nil, err
	}

	_, err = conn.Exec(`
		CREATE TABLE IF NOT EXISTS transcriptions (
			id         INTEGER PRIMARY KEY,
			room_slug  TEXT NOT NULL,
			session_id TEXT NOT NULL,
			data       TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		conn.Close()
		return nil, err
	}

	_, _ = conn.Exec("ALTER TABLE rooms ADD COLUMN transcription BOOLEAN DEFAULT FALSE")
	_, _ = conn.Exec("ALTER TABLE recordings ADD COLUMN kind TEXT NOT NULL DEFAULT 'recording'")
	// Add activated_at column if it doesn't exist
	_, _ = conn.Exec("ALTER TABLE rooms ADD COLUMN activated_at DATETIME")
	// Deactivate stale rooms from before migration
	_, _ = conn.Exec("UPDATE rooms SET active = FALSE WHERE active = TRUE AND activated_at IS NULL")
	return &DB{conn: conn}, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) CreateRoom(slug, hostHash string) error {
	_, err := db.conn.Exec(
		"INSERT INTO rooms (slug, host_hash) VALUES (?, ?)",
		slug, hostHash,
	)
	return err
}

func (db *DB) GetRoom(slug string) (*Room, error) {
	row := db.conn.QueryRow(
		"SELECT id, slug, host_hash, active, transcription, created_at FROM rooms WHERE slug = ?",
		slug,
	)
	var r Room
	err := row.Scan(&r.ID, &r.Slug, &r.HostHash, &r.Active, &r.Transcription, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *DB) ListRooms() ([]Room, error) {
	rows, err := db.conn.Query(
		"SELECT id, slug, host_hash, active, transcription, created_at FROM rooms ORDER BY created_at DESC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rooms []Room
	for rows.Next() {
		var r Room
		if err := rows.Scan(&r.ID, &r.Slug, &r.HostHash, &r.Active, &r.Transcription, &r.CreatedAt); err != nil {
			return nil, err
		}
		rooms = append(rooms, r)
	}
	return rooms, rows.Err()
}

func (db *DB) DeleteRoom(slug string) error {
	_, err := db.conn.Exec("DELETE FROM rooms WHERE slug = ?", slug)
	return err
}

func (db *DB) SetTranscription(slug string, enabled bool) error {
	_, err := db.conn.Exec("UPDATE rooms SET transcription = ? WHERE slug = ?", enabled, slug)
	return err
}

func (db *DB) SetRoomActive(slug string, active bool) error {
	if active {
		_, err := db.conn.Exec("UPDATE rooms SET active = ?, activated_at = CURRENT_TIMESTAMP WHERE slug = ?", active, slug)
		return err
	}
	_, err := db.conn.Exec("UPDATE rooms SET active = ?, activated_at = NULL WHERE slug = ?", active, slug)
	return err
}

type Recording struct {
	ID          int64
	RoomSlug    string
	Kind        string
	DownloadURL string
	ExpiresAt   time.Time
	DurationSec int
	CreatedAt   time.Time
}

func (db *DB) AddRecording(roomSlug, kind, downloadURL string, expiresAt time.Time, durationSec int) error {
	_, err := db.conn.Exec(
		"INSERT INTO recordings (room_slug, kind, download_url, expires_at, duration_sec) VALUES (?, ?, ?, ?, ?)",
		roomSlug, kind, downloadURL, expiresAt, durationSec,
	)
	return err
}

func (db *DB) ListRecordings(roomSlug string) ([]Recording, error) {
	rows, err := db.conn.Query(
		"SELECT id, room_slug, kind, download_url, expires_at, duration_sec, created_at FROM recordings WHERE room_slug = ? AND kind = 'recording' AND expires_at > ? ORDER BY created_at DESC",
		roomSlug, time.Now(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var recs []Recording
	for rows.Next() {
		var r Recording
		if err := rows.Scan(&r.ID, &r.RoomSlug, &r.Kind, &r.DownloadURL, &r.ExpiresAt, &r.DurationSec, &r.CreatedAt); err != nil {
			return nil, err
		}
		recs = append(recs, r)
	}
	return recs, rows.Err()
}

type Transcription struct {
	ID        int64
	RoomSlug  string
	SessionID string
	Data      string
	CreatedAt time.Time
}

func (db *DB) AddTranscription(roomSlug, sessionID, data string) error {
	_, err := db.conn.Exec(
		"INSERT INTO transcriptions (room_slug, session_id, data) VALUES (?, ?, ?)",
		roomSlug, sessionID, data,
	)
	return err
}

func (db *DB) ListTranscriptions(roomSlug string) ([]Transcription, error) {
	rows, err := db.conn.Query(
		"SELECT id, room_slug, session_id, '', created_at FROM transcriptions WHERE room_slug = ? ORDER BY created_at DESC",
		roomSlug,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []Transcription
	for rows.Next() {
		var t Transcription
		if err := rows.Scan(&t.ID, &t.RoomSlug, &t.SessionID, &t.Data, &t.CreatedAt); err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

func (db *DB) GetTranscription(id int64) (*Transcription, error) {
	row := db.conn.QueryRow(
		"SELECT id, room_slug, session_id, data, created_at FROM transcriptions WHERE id = ?",
		id,
	)
	var t Transcription
	if err := row.Scan(&t.ID, &t.RoomSlug, &t.SessionID, &t.Data, &t.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (db *DB) DeactivateExpiredRooms(maxAge time.Duration) error {
	_, err := db.conn.Exec(
		"UPDATE rooms SET active = FALSE, activated_at = NULL WHERE active = TRUE AND activated_at < ?",
		time.Now().Add(-maxAge),
	)
	return err
}
