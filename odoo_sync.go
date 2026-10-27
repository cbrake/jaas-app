package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const odooSyncMigration = `
CREATE TABLE odoo_sync_identity (
 id INTEGER PRIMARY KEY CHECK(id=1), source_id TEXT NOT NULL,
 base_url TEXT NOT NULL, database_name TEXT NOT NULL
);
CREATE TABLE odoo_sync_outbox (
 minutes_id INTEGER PRIMARY KEY,
 revision INTEGER NOT NULL CHECK(revision>0),
 generation INTEGER NOT NULL DEFAULT 1 CHECK(generation>0),
 operation TEXT NOT NULL CHECK(operation IN ('upsert','delete')),
 status TEXT NOT NULL CHECK(status IN ('pending','processing','sent','failed')),
 attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt_at INTEGER NOT NULL DEFAULT 0,
 locked_until INTEGER NOT NULL DEFAULT 0,
 lease_token TEXT NOT NULL DEFAULT '',
 error_code TEXT NOT NULL DEFAULT '',
 delivered_at INTEGER
);
CREATE INDEX odoo_sync_due ON odoo_sync_outbox(status,next_attempt_at);
-- A reused INTEGER PRIMARY KEY is a different meeting even when a pending
-- deletion is coalesced before delivery. Preserve that identity boundary.
CREATE TRIGGER odoo_sync_minutes_insert AFTER INSERT ON minutes
WHEN (NEW.status='ready' AND NEW.cancelled_at IS NULL)
 OR EXISTS(SELECT 1 FROM odoo_sync_outbox WHERE minutes_id=NEW.id)
BEGIN
 INSERT INTO odoo_sync_outbox(minutes_id,revision,operation,status)
 VALUES(NEW.id,1,CASE WHEN NEW.status='ready' AND NEW.cancelled_at IS NULL THEN 'upsert' ELSE 'delete' END,'pending')
 ON CONFLICT(minutes_id) DO UPDATE SET revision=revision+1,generation=generation+1,operation=excluded.operation,
 status='pending',attempts=0,next_attempt_at=0,locked_until=0,lease_token='',error_code='';
END;
CREATE TRIGGER odoo_sync_minutes_update AFTER UPDATE OF status,result_json,model,cancelled_at,meeting_at ON minutes
WHEN (NEW.status IS NOT OLD.status OR NEW.result_json IS NOT OLD.result_json
 OR NEW.model IS NOT OLD.model OR NEW.cancelled_at IS NOT OLD.cancelled_at OR NEW.meeting_at IS NOT OLD.meeting_at)
 AND ((NEW.status='ready' AND NEW.cancelled_at IS NULL)
 OR EXISTS(SELECT 1 FROM odoo_sync_outbox WHERE minutes_id=NEW.id))
BEGIN
 INSERT INTO odoo_sync_outbox(minutes_id,revision,operation,status)
 VALUES(NEW.id,1,CASE WHEN NEW.status='ready' AND NEW.cancelled_at IS NULL THEN 'upsert' ELSE 'delete' END,'pending')
 ON CONFLICT(minutes_id) DO UPDATE SET revision=revision+1,operation=excluded.operation,
 status='pending',attempts=0,next_attempt_at=0,locked_until=0,lease_token='',error_code='';
END;
CREATE TRIGGER odoo_sync_minutes_delete AFTER DELETE ON minutes
WHEN EXISTS(SELECT 1 FROM odoo_sync_outbox WHERE minutes_id=OLD.id)
BEGIN
 UPDATE odoo_sync_outbox SET revision=revision+1,operation='delete',status='pending',
 attempts=0,next_attempt_at=0,locked_until=0,lease_token='',error_code=''
 WHERE minutes_id=OLD.id;
END;
-- Ownership revocation must not depend on mapping/accepting the new owner.
-- Retract first; an operator can explicitly enqueue after checking the mapping.
CREATE TRIGGER odoo_sync_room_owner AFTER UPDATE OF owner_email ON rooms
WHEN NEW.owner_email IS NOT OLD.owner_email
BEGIN
 UPDATE odoo_sync_outbox SET revision=revision+1,operation='delete',status='pending',attempts=0,
 next_attempt_at=0,locked_until=0,lease_token='',error_code=''
 WHERE operation='upsert' AND minutes_id IN (SELECT id FROM minutes WHERE room_slug=NEW.slug);
END;
CREATE TRIGGER odoo_sync_capture_metadata AFTER UPDATE OF client_name,project_name ON capture_runs
WHEN NEW.client_name IS NOT OLD.client_name OR NEW.project_name IS NOT OLD.project_name
BEGIN
 UPDATE odoo_sync_outbox SET revision=revision+1,status='pending',attempts=0,
 next_attempt_at=0,locked_until=0,lease_token='',error_code=''
 WHERE operation='upsert' AND minutes_id IN (
 SELECT id FROM minutes WHERE room_slug=NEW.room_slug AND session_id='captura-'||NEW.id);
END;`

var odooSourcePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,79}$`)

func loadOdooSyncConfig(cfg *Config) error {
	switch strings.TrimSpace(os.Getenv("ODOO_SYNC_ENABLED")) {
	case "", "false":
		return nil
	case "true":
		cfg.OdooSyncEnabled = true
	default:
		return errors.New("ODOO_SYNC_ENABLED must be true or false")
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(os.Getenv("ODOO_BASE_URL")), "/"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("ODOO_BASE_URL must be an HTTPS origin without credentials, path, query or fragment")
	}
	cfg.OdooBaseURL = u.String()
	cfg.OdooSourceID = strings.TrimSpace(os.Getenv("ODOO_SOURCE_ID"))
	if !odooSourcePattern.MatchString(cfg.OdooSourceID) {
		return errors.New("ODOO_SOURCE_ID must be a stable 8-80 character alphanumeric identifier (dash/underscore allowed)")
	}
	cfg.OdooDatabase = strings.TrimSpace(os.Getenv("ODOO_DATABASE"))
	if strings.ContainsAny(cfg.OdooDatabase, "\r\n") || len(cfg.OdooDatabase) > 128 {
		return errors.New("ODOO_DATABASE is invalid")
	}
	cfg.OdooAPIKey, err = readSecret("ODOO_API_KEY")
	if err != nil || cfg.OdooAPIKey == "" || strings.ContainsAny(cfg.OdooAPIKey, "\r\n") {
		return errors.New("ODOO_API_KEY or ODOO_API_KEY_FILE must supply a valid credential")
	}
	if path := strings.TrimSpace(os.Getenv("ODOO_OWNER_MAP_FILE")); path != "" {
		cfg.odooOwnerMap, err = loadOdooOwnerMap(path, cfg)
		if err != nil {
			return err
		}
	}
	return nil
}

// Binding makes a config typo fail closed instead of replaying existing private
// records into another database/source. Changing destination needs an explicit
// operator migration; neither the worker nor a retry command rewrites this row.
func (db *DB) bindOdooSyncIdentity(cfg *Config) error {
	if !cfg.OdooSyncEnabled {
		return nil
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return errors.New("cannot bind Odoo synchronization destination")
	}
	defer tx.Rollback()
	digest := cfg.odooOwnerMapDigest()
	_, err = tx.Exec(`INSERT INTO odoo_sync_identity(id,source_id,base_url,database_name,owner_map_digest)
 VALUES(1,?,?,?,?) ON CONFLICT(id) DO NOTHING`, cfg.OdooSourceID, cfg.OdooBaseURL, cfg.OdooDatabase, digest)
	if err != nil {
		return errors.New("cannot bind Odoo synchronization destination")
	}
	var source, base, database, boundDigest string
	if err := tx.QueryRow("SELECT source_id,base_url,database_name,owner_map_digest FROM odoo_sync_identity WHERE id=1").Scan(&source, &base, &database, &boundDigest); err != nil {
		return errors.New("cannot read Odoo synchronization destination")
	}
	if source != cfg.OdooSourceID || base != cfg.OdooBaseURL || database != cfg.OdooDatabase {
		return errors.New("Odoo synchronization destination changed; operator migration required")
	}
	// A v7 binding may already have sent the same revision, even if a manual
	// retry reset its attempts. It can only retain the original direct mode.
	if boundDigest == "" && cfg.odooOwnerMap == nil {
		if _, err := tx.Exec("UPDATE odoo_sync_identity SET owner_map_digest=? WHERE id=1", digest); err != nil {
			return errors.New("cannot bind Odoo owner correspondence")
		}
	} else if boundDigest != digest {
		return errors.New("Odoo owner correspondence changed; operator migration required")
	}
	if err := tx.Commit(); err != nil {
		return errors.New("cannot bind Odoo synchronization destination")
	}
	return nil
}

type odooSyncJob struct {
	MinutesID  int64
	Revision   int64
	Generation int64
	Operation  string
	Attempts   int
	Token      string
}

func (db *DB) claimOdooSync(lease time.Duration) (*odooSyncJob, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, errors.New("cannot create synchronization lease")
	}
	j := &odooSyncJob{Token: hex.EncodeToString(token[:])}
	now := db.unix()
	err := db.conn.QueryRow(`UPDATE odoo_sync_outbox SET status='processing',attempts=attempts+1,
 locked_until=?,lease_token=? WHERE minutes_id=(SELECT minutes_id FROM odoo_sync_outbox
 WHERE (status='pending' AND next_attempt_at<=?) OR (status='processing' AND locked_until<=?)
 ORDER BY next_attempt_at,minutes_id LIMIT 1)
 RETURNING minutes_id,revision,generation,operation,attempts`, now+int64(lease/time.Second), j.Token, now, now).
		Scan(&j.MinutesID, &j.Revision, &j.Generation, &j.Operation, &j.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

type odooSentiment struct {
	Overall     string `json:"overall"`
	Explanation string `json:"explanation"`
	Caveats     string `json:"caveats"`
}

type odooSnapshot struct {
	SchemaVersion           int                `json:"schema_version"`
	SourceID                string             `json:"source_id"`
	ExternalID              string             `json:"external_id"`
	Revision                int64              `json:"revision"`
	Generation              int64              `json:"generation"`
	Operation               string             `json:"operation"`
	MeetingAt               string             `json:"meeting_at"`
	OwnerEmail              string             `json:"owner_email"`
	RoomTitle               string             `json:"room_title"`
	ClientRef               string             `json:"client_ref"`
	ProjectRef              string             `json:"project_ref"`
	Summary                 string             `json:"summary"`
	Sentiment               *odooSentiment     `json:"sentiment"`
	Signals                 []CommercialSignal `json:"signals"`
	ModelVersion            string             `json:"model_version"`
	AnalysisVersion         string             `json:"analysis_version"`
	SpeakerIdentityVerified *bool              `json:"speaker_identity_verified"`
}

func (p odooSnapshot) MarshalJSON() ([]byte, error) {
	if p.Operation == "delete" {
		return json.Marshal(struct {
			SchemaVersion int    `json:"schema_version"`
			SourceID      string `json:"source_id"`
			ExternalID    string `json:"external_id"`
			Revision      int64  `json:"revision"`
			Generation    int64  `json:"generation"`
			Operation     string `json:"operation"`
		}{p.SchemaVersion, p.SourceID, p.ExternalID, p.Revision, p.Generation, p.Operation})
	}
	type plain odooSnapshot
	return json.Marshal(plain(p))
}

// Read the snapshot and matching lease in one SELECT. No lock/transaction is
// held across HTTP. An update after this read queues a strictly newer revision.
func (db *DB) buildOdooSnapshot(j *odooSyncJob, source string) (*odooSnapshot, error) {
	p := &odooSnapshot{SchemaVersion: 1, SourceID: source, ExternalID: strconv.FormatInt(j.MinutesID, 10), Revision: j.Revision, Operation: j.Operation}
	var operation string
	var result, model, room, owner, client, project, status sql.NullString
	var meeting, cancelled sql.NullInt64
	err := db.conn.QueryRow(`SELECT o.operation,o.generation,m.result_json,m.model,m.room_slug,r.owner_email,
 c.client_name,c.project_name,m.status,m.meeting_at,m.cancelled_at
 FROM odoo_sync_outbox o LEFT JOIN minutes m ON m.id=o.minutes_id
 LEFT JOIN rooms r ON r.slug=m.room_slug
 LEFT JOIN capture_runs c ON c.room_slug=m.room_slug AND m.session_id='captura-'||c.id
 WHERE o.minutes_id=? AND o.revision=? AND o.status='processing' AND o.lease_token=?`,
		j.MinutesID, j.Revision, j.Token).Scan(&operation, &p.Generation, &result, &model, &room, &owner, &client, &project, &status, &meeting, &cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound // newer revision or lease superseded this claim
	}
	if err != nil {
		return nil, errors.New("snapshot_read_failed")
	}
	if operation == "delete" {
		return p, nil // tombstones contain only identifiers and revision
	}
	if !status.Valid || status.String != "ready" || cancelled.Valid {
		return nil, errors.New("snapshot_state_invalid")
	}
	if normalizeEmail(owner.String) == "" {
		return nil, errors.New("snapshot_owner_missing")
	}
	if len(result.String) > 256<<10 {
		return nil, errors.New("snapshot_result_too_large")
	}
	var minutes MinutesResult
	if err := json.Unmarshal([]byte(result.String), &minutes); err != nil {
		return nil, errors.New("snapshot_result_invalid")
	}
	if utf8.RuneCountInString(minutes.Summary) > 10000 || utf8.RuneCountInString(minutes.Sentiment.Explanation) > 2000 || utf8.RuneCountInString(minutes.Sentiment.Caveats) > 2000 || len(minutes.CommercialSignals) > 12 || len(model.String) > 128 {
		return nil, errors.New("snapshot_field_too_large")
	}
	for _, field := range []string{room.String, client.String, project.String} {
		if utf8.RuneCountInString(field) > 200 {
			return nil, errors.New("snapshot_metadata_too_large")
		}
	}
	p.MeetingAt = time.Unix(meeting.Int64, 0).UTC().Format(time.RFC3339)
	p.OwnerEmail, p.RoomTitle = normalizeEmail(owner.String), room.String
	p.ClientRef, p.ProjectRef = client.String, project.String
	p.Summary, p.ModelVersion = minutes.Summary, model.String
	p.AnalysisVersion = "meet-commercial-v1"
	p.Sentiment = &odooSentiment{minutes.Sentiment.Overall, minutes.Sentiment.Explanation, minutes.Sentiment.Caveats}
	p.Signals = minutes.CommercialSignals
	if p.Signals == nil {
		p.AnalysisVersion = "meet-minutes-legacy"
		p.Signals = []CommercialSignal{}
	}
	verified := false
	p.SpeakerIdentityVerified = &verified
	return p, nil
}

func (db *DB) finishOdooSync(j *odooSyncJob, code string, retry bool) error {
	status := "sent"
	next := int64(0)
	var delivered any = db.unix()
	if code != "" {
		status, delivered = "failed", nil
		if retry && j.Attempts < 8 {
			status = "pending"
			delay := 30 * time.Second * time.Duration(1<<min(j.Attempts-1, 7))
			if delay > time.Hour {
				delay = time.Hour
			}
			next = db.now().Add(delay).Unix()
		}
	}
	_, err := db.conn.Exec(`UPDATE odoo_sync_outbox SET status=?,next_attempt_at=?,locked_until=0,
 lease_token='',error_code=?,delivered_at=? WHERE minutes_id=? AND revision=? AND lease_token=? AND status='processing'`,
		status, next, code, delivered, j.MinutesID, j.Revision, j.Token)
	return err
}

type odooSyncClient struct {
	endpoint, key, database string
	http                    *http.Client
}

func newOdooSyncClient(cfg *Config) *odooSyncClient {
	return &odooSyncClient{
		endpoint: cfg.OdooBaseURL + "/json/2/transgenia.meet.session/ingest_snapshot",
		key:      cfg.OdooAPIKey, database: cfg.OdooDatabase,
		http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

// Returns only a fixed code and retry decision; no remote error text, URLs,
// response bodies, headers or credentials cross this logging boundary.
func (c *odooSyncClient) send(ctx context.Context, p *odooSnapshot) (string, bool) {
	body, err := json.Marshal(struct {
		Payload *odooSnapshot `json:"payload"`
	}{p})
	if err != nil || len(body) > 128<<10 {
		return "payload_invalid", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "request_invalid", false
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Meet-Odoo-Sync/1")
	if c.database != "" {
		req.Header.Set("X-Odoo-Database", c.database)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "transport_error", true
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "http_" + strconv.Itoa(resp.StatusCode), retry
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(data) > 4096 {
		return "ack_invalid", true
	}
	var ack struct {
		SourceID        string `json:"source_id"`
		ExternalID      string `json:"external_id"`
		Revision        int64  `json:"revision"`
		Status          string `json:"status"`
		CurrentRevision *int64 `json:"current_revision,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ack) != nil || ack.SourceID != p.SourceID || ack.ExternalID != p.ExternalID || ack.Revision != p.Revision || (ack.Status != "applied" && ack.Status != "duplicate" && ack.Status != "stale") {
		return "ack_invalid", true
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || (ack.CurrentRevision != nil && (*ack.CurrentRevision < p.Revision || (ack.Status != "stale" && *ack.CurrentRevision != p.Revision))) {
		return "ack_invalid", true
	}
	return "", false
}

func (app *App) processOdooSync(ctx context.Context, client *odooSyncClient) bool {
	j, err := app.DB.claimOdooSync(90 * time.Second)
	if errors.Is(err, ErrNotFound) {
		return false
	}
	if err != nil {
		slog.Error("Odoo synchronization claim failed")
		return false
	}
	if j.Attempts > 8 {
		_ = app.DB.finishOdooSync(j, "attempt_limit", false)
		return true
	}
	p, err := app.DB.buildOdooSnapshot(j, app.Config.OdooSourceID)
	if errors.Is(err, ErrNotFound) {
		return true
	}
	code, retry := "", false
	if err != nil {
		code = "snapshot_invalid"
	} else if err := app.Config.mapOdooSnapshotOwner(p); err != nil {
		code = "owner_mapping_missing"
	} else {
		code, retry = client.send(ctx, p)
	}
	if err := app.DB.finishOdooSync(j, code, retry); err != nil {
		slog.Error("Odoo synchronization acknowledgment persistence failed")
	} else if code != "" {
		slog.Warn("Odoo synchronization pending or failed", "code", code, "attempt", j.Attempts)
	}
	return true
}

func (app *App) runOdooSync(ctx context.Context) {
	if !app.Config.OdooSyncEnabled {
		return
	}
	client := newOdooSyncClient(app.Config)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		// Limit each pass so shutdown and retry timing remain responsive.
		for count := 0; count < 25 && ctx.Err() == nil; count++ {
			if !app.processOdooSync(ctx, client) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// EnqueueOdooSync is an explicit, per-record history/replay operation. Enabling
// the integration or migrating the DB never enumerates old meetings.
func (db *DB) EnqueueOdooSync(id int64) error {
	res, err := db.conn.Exec(`INSERT INTO odoo_sync_outbox(minutes_id,revision,operation,status)
 SELECT id,1,'upsert','pending' FROM minutes WHERE id=? AND status='ready' AND cancelled_at IS NULL
 ON CONFLICT(minutes_id) DO UPDATE SET revision=revision+1,operation='upsert',status='pending',
 attempts=0,next_attempt_at=0,locked_until=0,lease_token='',error_code=''`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) RetryOdooSync(id int64) error {
	res, err := db.conn.Exec(`UPDATE odoo_sync_outbox SET status='pending',attempts=0,next_attempt_at=0,error_code=''
 WHERE minutes_id=? AND status='failed'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func cmdOdooSync(action string, args []string) error {
	if (action == "status" && len(args) != 0) || (action != "status" && len(args) != 1) {
		return errors.New("usage: odoo-sync-status | odoo-sync-enqueue ID | odoo-sync-retry ID")
	}
	var id int64
	if action != "status" {
		var err error
		id, err = strconv.ParseInt(args[0], 10, 64)
		if err != nil || id < 1 {
			return errors.New("minutes ID must be a positive integer")
		}
	}
	// CLI diagnostics/retries never apply schema migrations or create a database.
	// In particular, preflight status must not upgrade a live older runtime.
	mode := "rw"
	if action == "status" {
		mode = "ro"
	}
	conn, err := sql.Open("sqlite", "file:"+envDefault("DB_PATH", "./jaas.db")+"?mode="+mode+"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return errors.New("cannot open synchronization database")
	}
	db := &DB{conn: conn, now: time.Now}
	defer db.Close()
	var schema int
	if err := conn.QueryRow("PRAGMA user_version").Scan(&schema); err != nil || schema < 7 {
		return errors.New("synchronization database must already have schema v7 or later")
	}
	if action == "status" {
		rows, err := db.conn.Query("SELECT status,COUNT(*) FROM odoo_sync_outbox GROUP BY status ORDER BY status")
		if err != nil {
			return errors.New("cannot read synchronization status")
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			var count int64
			if err := rows.Scan(&state, &count); err != nil {
				return err
			}
			fmt.Printf("%s %d\n", state, count)
		}
		return rows.Err()
	}
	if action == "enqueue" {
		return db.EnqueueOdooSync(id)
	}
	return db.RetryOdooSync(id)
}
