package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminIdleTimeout     = 2 * time.Hour
	adminAbsoluteTimeout = 12 * time.Hour
	bcryptCost           = 12
	minAdminPasswordLen  = 14
	minHostPasswordLen   = 10
)

// dummyHash keeps the login timing similar when no credential exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equaliser-not-a-password"), bcryptCost)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func tokensEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// cookieName applies the __Host- prefix whenever cookies are Secure, which
// pins them to this exact host and path "/".
func (app *App) cookieName(base string) string {
	if app.Config.SecureCookies {
		return "__Host-" + base
	}
	return base
}

func (app *App) setCookie(w http.ResponseWriter, base, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     app.cookieName(base),
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   app.Config.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (app *App) clearCookie(w http.ResponseWriter, base string) {
	http.SetCookie(w, &http.Cookie{
		Name:     app.cookieName(base),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   app.Config.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (app *App) cookieValue(r *http.Request, base string) string {
	c, err := r.Cookie(app.cookieName(base))
	if err != nil {
		return ""
	}
	return c.Value
}

// --- admin credential ---

type AdminCredential struct {
	Hash       string
	MustChange bool
}

func (db *DB) GetAdminCredential() (*AdminCredential, error) {
	var c AdminCredential
	err := db.conn.QueryRow("SELECT password_hash, must_change FROM admin_credentials WHERE id = 1").Scan(&c.Hash, &c.MustChange)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// SetAdminPassword replaces the admin credential and revokes every admin
// session, so a reset or change logs out all browsers.
func (db *DB) SetAdminPassword(password string, mustChange bool) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO admin_credentials (id, password_hash, must_change, updated_at) VALUES (1, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET password_hash = excluded.password_hash, must_change = excluded.must_change, updated_at = excluded.updated_at`,
		string(hash), mustChange, db.unix(),
	); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM admin_sessions"); err != nil {
		return err
	}
	return tx.Commit()
}

// BootstrapAdmin hashes the initial password file into the database the
// first time the service starts. Later starts ignore the file.
func (db *DB) BootstrapAdmin(path, legacy string) (bool, error) {
	if _, err := db.GetAdminCredential(); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	var pw string
	switch {
	case path != "":
		b, err := os.ReadFile(path)
		if err != nil {
			return false, fmt.Errorf("reading ADMIN_INITIAL_PASSWORD_FILE: %w", err)
		}
		pw = strings.TrimSpace(string(b))
	case strings.TrimSpace(legacy) != "":
		pw = strings.TrimSpace(legacy)
	default:
		return false, fmt.Errorf("no admin credential yet and neither ADMIN_INITIAL_PASSWORD_FILE nor ADMIN_PASSWORD is set")
	}
	if len(pw) < minAdminPasswordLen {
		return false, fmt.Errorf("initial admin password must be at least %d characters", minAdminPasswordLen)
	}
	return true, db.SetAdminPassword(pw, true)
}

func validateNewAdminPassword(current, next string) error {
	if len(next) < minAdminPasswordLen {
		return fmt.Errorf("The new password must be at least %d characters long.", minAdminPasswordLen)
	}
	if next == current {
		return fmt.Errorf("The new password must be different from the current one.")
	}
	return nil
}

// --- admin sessions ---

// AdminSession is a panel session. Email is the Cloudflare Access identity
// (empty in password mode); Superadmin is resolved per request.
type AdminSession struct {
	TokenHash  string
	CSRFToken  string
	MustChange bool
	Email      string
	Superadmin bool
}

func (db *DB) CreateAdminSession(email string) (token string, err error) {
	token = randomToken()
	now := db.now()
	_, err = db.conn.Exec(
		"INSERT INTO admin_sessions (token_hash, csrf_token, email, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)",
		hashToken(token), randomToken(), email, now.Unix(), now.Unix(), now.Add(adminAbsoluteTimeout).Unix(),
	)
	return token, err
}

func (db *DB) GetAdminSession(token string) (*AdminSession, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	h := hashToken(token)
	now := db.now()
	var s AdminSession
	var lastSeen int64
	err := db.conn.QueryRow(
		`SELECT s.token_hash, s.csrf_token, s.email, s.last_seen_at, COALESCE(c.must_change, 1)
		 FROM admin_sessions s LEFT JOIN admin_credentials c ON c.id = 1
		 WHERE s.token_hash = ? AND s.expires_at > ?`,
		h, now.Unix(),
	).Scan(&s.TokenHash, &s.CSRFToken, &s.Email, &lastSeen, &s.MustChange)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if now.Sub(time.Unix(lastSeen, 0)) > adminIdleTimeout {
		db.DeleteAdminSession(token)
		return nil, ErrNotFound
	}
	if now.Sub(time.Unix(lastSeen, 0)) > time.Minute {
		db.conn.Exec("UPDATE admin_sessions SET last_seen_at = ? WHERE token_hash = ?", now.Unix(), h)
	}
	return &s, nil
}

func (db *DB) DeleteAdminSession(token string) error {
	_, err := db.conn.Exec("DELETE FROM admin_sessions WHERE token_hash = ?", hashToken(token))
	return err
}

// --- participant sessions ---

// ParticipantSession replaces the upstream "?jwt=...&mod=1" redirect: the
// browser only holds an opaque cookie and the JaaS JWT is minted server side
// each time the meeting page is rendered.
type ParticipantSession struct {
	RoomSlug    string
	DisplayName string
	Email       string
	Moderator   bool
	CSRFToken   string
}

func (db *DB) CreateParticipantSession(slug, displayName, email string, moderator bool, lifetime time.Duration) (string, error) {
	token := randomToken()
	now := db.now()
	_, err := db.conn.Exec(
		"INSERT INTO participant_sessions (token_hash, room_slug, display_name, email, moderator, csrf_token, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		hashToken(token), slug, displayName, email, moderator, randomToken(), now.Unix(), now.Add(lifetime).Unix(),
	)
	return token, err
}

func (db *DB) GetParticipantSession(token string) (*ParticipantSession, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	var p ParticipantSession
	err := db.conn.QueryRow(
		"SELECT room_slug, display_name, email, moderator, csrf_token FROM participant_sessions WHERE token_hash = ? AND expires_at > ?",
		hashToken(token), db.unix(),
	).Scan(&p.RoomSlug, &p.DisplayName, &p.Email, &p.Moderator, &p.CSRFToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &p, err
}

func (db *DB) DeleteParticipantSession(token string) error {
	_, err := db.conn.Exec("DELETE FROM participant_sessions WHERE token_hash = ?", hashToken(token))
	return err
}
