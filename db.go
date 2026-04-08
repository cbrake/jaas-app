package main

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Room struct {
	ID        int64
	Slug      string
	HostHash  string
	Active    bool
	CreatedAt time.Time
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
		"SELECT id, slug, host_hash, active, created_at FROM rooms WHERE slug = ?",
		slug,
	)
	var r Room
	err := row.Scan(&r.ID, &r.Slug, &r.HostHash, &r.Active, &r.CreatedAt)
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
		"SELECT id, slug, host_hash, active, created_at FROM rooms ORDER BY created_at DESC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rooms []Room
	for rows.Next() {
		var r Room
		if err := rows.Scan(&r.ID, &r.Slug, &r.HostHash, &r.Active, &r.CreatedAt); err != nil {
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

func (db *DB) SetRoomActive(slug string, active bool) error {
	_, err := db.conn.Exec("UPDATE rooms SET active = ? WHERE slug = ?", active, slug)
	return err
}
