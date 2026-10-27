package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const odooTestResult = `{"summary":"Necesidad discutida; pendiente de revisión.","sentiment":{"overall":"neutral","explanation":"No expresa una decisión de compra.","caveats":"Interpretación textual."},"commercial_signals":[]}`

func odooTestDB(t *testing.T) (*DB, *time.Time) {
	t.Helper()
	db, err := InitDB(filepath.Join(t.TempDir(), "meet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	db.now = func() time.Time { return now }
	if err := db.CreateRoom("customer-review", "test-hash", "owner@example.test"); err != nil {
		t.Fatal(err)
	}
	return db, &now
}

func seedOdooMinutes(t *testing.T, db *DB, id int64, result string) {
	t.Helper()
	_, err := db.conn.Exec(`INSERT INTO minutes(id,room_slug,session_id,status,next_attempt_at,meeting_at,result_json,model,created_at,updated_at)
 VALUES(?,'customer-review',?,'ready',?,?,?,?,?,?)`, id, "session-"+itoa(id), db.unix(), db.unix(), result, "test-model", db.unix(), db.unix())
	if err != nil {
		t.Fatal(err)
	}
}

func mustClaimOdoo(t *testing.T, db *DB) *odooSyncJob {
	t.Helper()
	j, err := db.claimOdooSync(90 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestOdooOutboxRevisionCancellationAndIDReuse(t *testing.T) {
	db, _ := odooTestDB(t)
	seedOdooMinutes(t, db, 1, odooTestResult)
	first := mustClaimOdoo(t, db)
	if first.Revision != 1 || first.Generation != 1 || first.Operation != "upsert" {
		t.Fatalf("first claim: %+v", first)
	}
	if _, err := db.conn.Exec("UPDATE minutes SET result_json=? WHERE id=1", strings.Replace(odooTestResult, "Necesidad", "Nueva necesidad", 1)); err != nil {
		t.Fatal(err)
	}
	if err := db.finishOdooSync(first, "", false); err != nil {
		t.Fatal(err)
	}
	second := mustClaimOdoo(t, db)
	if second.Revision != 2 {
		t.Fatalf("stale ack lost revision: %+v", second)
	}
	if err := db.CancelMinutes(1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.buildOdooSnapshot(second, "meet-test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("superseded snapshot: %v", err)
	}
	third := mustClaimOdoo(t, db)
	if third.Revision != 3 || third.Operation != "delete" {
		t.Fatalf("cancellation: %+v", third)
	}
	p, err := db.buildOdooSnapshot(third, "meet-test")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(p)
	var tombstone map[string]any
	if json.Unmarshal(data, &tombstone) != nil || len(tombstone) != 6 || tombstone["generation"] != float64(1) || strings.Contains(string(data), "owner") || strings.Contains(string(data), "summary") {
		t.Fatalf("tombstone leaked content: %s", data)
	}
	if err := db.finishOdooSync(third, "", false); err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueOdooSync(1); !errors.Is(err, ErrNotFound) {
		t.Fatal("cancelled minutes requeued")
	}
	if _, err := db.conn.Exec("DELETE FROM minutes WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	fourth := mustClaimOdoo(t, db)
	if fourth.Revision != 4 || fourth.Operation != "delete" {
		t.Fatalf("deletion: %+v", fourth)
	}
	// INTEGER PRIMARY KEY may reuse the largest deleted ID; revision cannot reset.
	seedOdooMinutes(t, db, 1, odooTestResult)
	if err := db.finishOdooSync(fourth, "", false); err != nil {
		t.Fatal(err)
	}
	fifth := mustClaimOdoo(t, db)
	if fifth.Revision != 5 || fifth.Generation != 2 || fifth.Operation != "upsert" {
		t.Fatalf("ID reuse: %+v", fifth)
	}
	if err := db.finishOdooSync(fifth, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.claimOdooSync(time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("acknowledged event still claimable")
	}
}

func TestOdooCoalescedIDReuseChangesGenerationBeforeReady(t *testing.T) {
	db, _ := odooTestDB(t)
	seedOdooMinutes(t, db, 1, odooTestResult)
	first := mustClaimOdoo(t, db)
	original, err := db.buildOdooSnapshot(first, "meet-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.finishOdooSync(first, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.Exec("DELETE FROM minutes WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	// The new meeting reuses the numeric ID but starts queued, as in the real
	// pipeline. No consumer sees the intermediate deletion or queued event.
	_, err = db.conn.Exec(`INSERT INTO minutes(id,room_slug,session_id,status,next_attempt_at,meeting_at,created_at,updated_at)
 VALUES(1,'customer-review','new-session','queued',?,?,?,?)`, db.unix(), db.unix(), db.unix(), db.unix())
	if err != nil {
		t.Fatal(err)
	}
	var revision, generation int64
	var operation string
	if err := db.conn.QueryRow("SELECT revision,generation,operation FROM odoo_sync_outbox WHERE minutes_id=1").Scan(&revision, &generation, &operation); err != nil {
		t.Fatal(err)
	}
	if revision != 3 || generation != 2 || operation != "delete" {
		t.Fatalf("queued ID reuse lost identity: %d/%d/%s", revision, generation, operation)
	}
	if _, err := db.conn.Exec("UPDATE minutes SET status='ready',result_json=?,model='test-model' WHERE id=1", odooTestResult); err != nil {
		t.Fatal(err)
	}
	latest := mustClaimOdoo(t, db)
	replacement, err := db.buildOdooSnapshot(latest, "meet-test")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Revision != 4 || replacement.Generation != 2 || replacement.Operation != "upsert" {
		t.Fatalf("new meeting snapshot: %+v", replacement)
	}
	if replacement.OwnerEmail != original.OwnerEmail || replacement.Summary != original.Summary {
		t.Fatal("fixture must retain identical owner/evidence while changing identity")
	}
	if original.Generation != 1 {
		t.Fatal("initial generation not recorded")
	}
	if err := db.finishOdooSync(latest, "", false); err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueOdooSync(1); err != nil {
		t.Fatal(err)
	}
	replay := mustClaimOdoo(t, db)
	if replay.Generation != 2 || replay.Revision != 5 {
		t.Fatal("explicit replay must retain generation")
	}
}

func TestOdooLeaseRecoveryRejectsOldToken(t *testing.T) {
	db, now := odooTestDB(t)
	seedOdooMinutes(t, db, 1, odooTestResult)
	first := mustClaimOdoo(t, db)
	if _, err := db.claimOdooSync(time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("live lease was claimed")
	}
	*now = now.Add(91 * time.Second)
	second := mustClaimOdoo(t, db)
	if first.Token == second.Token || second.Attempts != 2 || second.Revision != first.Revision {
		t.Fatal("lease recovery invalid")
	}
	if err := db.finishOdooSync(first, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.buildOdooSnapshot(second, "meet-test"); err != nil {
		t.Fatalf("old token acknowledged new lease: %v", err)
	}
}

func TestOdooRetryBoundAndExplicitRetry(t *testing.T) {
	db, now := odooTestDB(t)
	seedOdooMinutes(t, db, 1, odooTestResult)
	for attempt := 1; attempt <= 8; attempt++ {
		j := mustClaimOdoo(t, db)
		if j.Attempts != attempt {
			t.Fatalf("attempt=%d", j.Attempts)
		}
		if err := db.finishOdooSync(j, "transport_error", true); err != nil {
			t.Fatal(err)
		}
		if _, err := db.claimOdooSync(time.Minute); !errors.Is(err, ErrNotFound) {
			t.Fatal("backoff not enforced")
		}
		*now = now.Add(2 * time.Hour)
	}
	var state string
	db.conn.QueryRow("SELECT status FROM odoo_sync_outbox WHERE minutes_id=1").Scan(&state)
	if state != "failed" {
		t.Fatalf("retry exhaustion=%s", state)
	}
	if err := db.RetryOdooSync(1); err != nil {
		t.Fatal(err)
	}
	j := mustClaimOdoo(t, db)
	if j.Revision != 1 || j.Attempts != 1 {
		t.Fatal("explicit retry must preserve idempotency revision")
	}
}

func TestOdooSnapshotsDistinguishLegacyAndAnalyzedEmpty(t *testing.T) {
	db, _ := odooTestDB(t)
	legacy := strings.Replace(odooTestResult, `,"commercial_signals":[]`, "", 1)
	seedOdooMinutes(t, db, 1, legacy)
	seedOdooMinutes(t, db, 2, odooTestResult)
	for _, want := range []string{"meet-minutes-legacy", "meet-commercial-v1"} {
		j := mustClaimOdoo(t, db)
		p, err := db.buildOdooSnapshot(j, "meet-test")
		if err != nil {
			t.Fatal(err)
		}
		if p.AnalysisVersion != want {
			t.Fatalf("analysis=%q want=%q", p.AnalysisVersion, want)
		}
		data, _ := json.Marshal(p)
		if !bytes.Contains(data, []byte(`"signals":[]`)) || !bytes.Contains(data, []byte(`"speaker_identity_verified":false`)) {
			t.Fatalf("missing evidence state: %s", data)
		}
		if p.OwnerEmail != "owner@example.test" || p.RoomTitle != "customer-review" {
			t.Fatal("source metadata wrong")
		}
		if err := db.finishOdooSync(j, "", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.conn.Exec("UPDATE rooms SET owner_email='changed@example.test' WHERE slug='customer-review'"); err != nil {
		t.Fatal(err)
	}
	j := mustClaimOdoo(t, db)
	p, err := db.buildOdooSnapshot(j, "meet-test")
	if err != nil {
		t.Fatal(err)
	}
	if j.Revision != 2 || p.Operation != "delete" || p.OwnerEmail != "" {
		t.Fatal("owner reassignment must retract before granting a new owner access")
	}
	if err := db.finishOdooSync(j, "", false); err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueOdooSync(j.MinutesID); err != nil {
		t.Fatal(err)
	}
	j = mustClaimOdoo(t, db)
	p, err = db.buildOdooSnapshot(j, "meet-test")
	if err != nil {
		t.Fatal(err)
	}
	if p.Revision != 3 || p.Operation != "upsert" || p.OwnerEmail != "changed@example.test" {
		t.Fatal("explicit enqueue did not publish mapped new owner")
	}
}

func TestOdooMigrationDoesNotBackfillHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:6] {
		if _, err := conn.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	_, err = conn.Exec(`INSERT INTO rooms(slug,host_hash,owner_email,created_at) VALUES('old','hash','owner@example.test',1);
 INSERT INTO minutes(room_slug,session_id,status,next_attempt_at,meeting_at,created_at,updated_at,result_json) VALUES('old','old-session','ready',1,1,1,1,'{}');
 PRAGMA user_version=6;`)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	t.Setenv("DB_PATH", path)
	if err := cmdOdooSync("status", nil); err == nil {
		t.Fatal("status unexpectedly accepted an old schema")
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := check.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	check.Close()
	if version != 6 {
		t.Fatalf("status command changed schema to %d", version)
	}
	db, err := InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.claimOdooSync(time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("migration enqueued historical meeting")
	}
	if err := db.EnqueueOdooSync(1); err != nil {
		t.Fatal(err)
	}
	if j := mustClaimOdoo(t, db); j.MinutesID != 1 {
		t.Fatal("explicit history enqueue failed")
	}
}

func TestOdooRealRoomDeletionAndRetentionRetract(t *testing.T) {
	for _, mode := range []string{"room", "retention"} {
		t.Run(mode, func(t *testing.T) {
			db, now := odooTestDB(t)
			seedOdooMinutes(t, db, 1, odooTestResult)
			j := mustClaimOdoo(t, db)
			if err := db.finishOdooSync(j, "", false); err != nil {
				t.Fatal(err)
			}
			if mode == "room" {
				if err := db.DeleteRoom("customer-review"); err != nil {
					t.Fatal(err)
				}
			} else {
				*now = now.Add(48 * time.Hour)
				if _, err := db.PurgeExpired(Retention{}, Retention{Days: 1}); err != nil {
					t.Fatal(err)
				}
			}
			j = mustClaimOdoo(t, db)
			p, err := db.buildOdooSnapshot(j, "meet-test")
			if err != nil {
				t.Fatal(err)
			}
			if p.Operation != "delete" || p.Revision != 2 || p.OwnerEmail != "" || p.Summary != "" {
				t.Fatalf("deletion not propagated: %+v", p)
			}
		})
	}
}

func TestOdooConfigOptInAndDestinationBinding(t *testing.T) {
	t.Setenv("ODOO_SYNC_ENABLED", "false")
	t.Setenv("ODOO_API_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	cfg := &Config{}
	if err := loadOdooSyncConfig(cfg); err != nil || cfg.OdooSyncEnabled || cfg.OdooAPIKey != "" {
		t.Fatal("disabled integration read credential")
	}
	t.Setenv("ODOO_SYNC_ENABLED", "true")
	t.Setenv("ODOO_SOURCE_ID", "meet-test-source")
	t.Setenv("ODOO_DATABASE", "test-db")
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, []byte("test-secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ODOO_API_KEY_FILE", keyPath)
	for _, bad := range []string{"http://odoo.example.test", "https://user:pass@odoo.example.test", "https://odoo.example.test/path", "https://odoo.example.test?key=bad", "https://odoo.example.test#fragment"} {
		t.Setenv("ODOO_BASE_URL", bad)
		if err := loadOdooSyncConfig(&Config{}); err == nil || strings.Contains(err.Error(), "test-secret-value") {
			t.Fatalf("accepted URL: %s", bad)
		}
	}
	t.Setenv("ODOO_BASE_URL", "https://odoo.example.test/")
	if err := loadOdooSyncConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.OdooAPIKey != "test-secret-value" || cfg.OdooBaseURL != "https://odoo.example.test" {
		t.Fatal("configuration not normalized")
	}
	db, _ := odooTestDB(t)
	if err := db.bindOdooSyncIdentity(cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.bindOdooSyncIdentity(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.OdooSourceID = "another-source"
	if err := db.bindOdooSyncIdentity(cfg); err == nil {
		t.Fatal("destination/source changed without migration")
	}
}

func odooTestClient(server *httptest.Server) *odooSyncClient {
	c := newOdooSyncClient(&Config{OdooBaseURL: server.URL, OdooAPIKey: "test-secret-value", OdooDatabase: "test-db"})
	c.http.Transport = server.Client().Transport
	return c
}

func TestOdooAmbiguousDeliveryReplaysSameRevision(t *testing.T) {
	db, now := odooTestDB(t)
	seedOdooMinutes(t, db, 1, odooTestResult)
	var calls atomic.Int32
	var materialized atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/2/transgenia.meet.session/ingest_snapshot" || r.Header.Get("Authorization") != "Bearer test-secret-value" || r.Header.Get("X-Odoo-Database") != "test-db" {
			t.Error("bad request contract")
		}
		var wire struct {
			Payload odooSnapshot `json:"payload"`
		}
		if json.NewDecoder(r.Body).Decode(&wire) != nil {
			t.Error("invalid payload")
			w.WriteHeader(400)
			return
		}
		status := "duplicate"
		if calls.Add(1) == 1 {
			materialized.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"source_id": wire.Payload.SourceID, "external_id": wire.Payload.ExternalID, "revision": wire.Payload.Revision, "status": status})
	}))
	defer server.Close()
	app := &App{DB: db, Config: &Config{OdooSourceID: "meet-test", OdooSyncEnabled: true}}
	client := odooTestClient(server)
	if !app.processOdooSync(context.Background(), client) {
		t.Fatal("first event missing")
	}
	*now = now.Add(time.Minute)
	if !app.processOdooSync(context.Background(), client) {
		t.Fatal("retry event missing")
	}
	if calls.Load() != 2 || materialized.Load() != 1 {
		t.Fatalf("calls=%d records=%d", calls.Load(), materialized.Load())
	}
	var status string
	db.conn.QueryRow("SELECT status FROM odoo_sync_outbox WHERE minutes_id=1").Scan(&status)
	if status != "sent" {
		t.Fatalf("status=%s", status)
	}
}

func TestOdooClientRejectsRedirectsAndInvalidAcknowledgments(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	p := &odooSnapshot{SchemaVersion: 1, SourceID: "meet-test", ExternalID: "1", Revision: 2, Generation: 1, Operation: "delete"}
	code, retry := odooTestClient(redirect).send(context.Background(), p)
	if code != "http_307" || retry || leaked.Load() != 0 {
		t.Fatal("redirect followed or retried")
	}
	for _, body := range []string{
		`{"source_id":"different","external_id":"1","revision":2,"status":"applied"}`,
		`{"source_id":"meet-test","external_id":"1","revision":1,"status":"applied"}`,
		`{"source_id":"meet-test","external_id":"1","revision":2,"status":"applied"} {"extra":1}`,
		`{"source_id":"meet-test","external_id":"1","revision":2,"status":"stale","current_revision":1}`,
		strings.Repeat("x", 4097),
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		code, retry := odooTestClient(server).send(context.Background(), p)
		server.Close()
		if code != "ack_invalid" || !retry {
			t.Fatalf("invalid ack accepted: %q", body[:min(80, len(body))])
		}
	}
}

func TestOdooWorkerLogsNoRemoteContent(t *testing.T) {
	db, _ := odooTestDB(t)
	seedOdooMinutes(t, db, 1, odooTestResult)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "private customer transcript test-secret-value")
	}))
	defer server.Close()
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	app := &App{DB: db, Config: &Config{OdooSyncEnabled: true, OdooSourceID: "meet-test"}}
	app.processOdooSync(context.Background(), odooTestClient(server))
	for _, private := range []string{"private customer", "test-secret-value", "owner@example.test", "Necesidad", server.URL} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("log disclosure: %s", logs.String())
		}
	}
	if !strings.Contains(logs.String(), "http_500") {
		t.Fatal("sanitized status missing")
	}
}

func TestOdooDisabledWorkerAndCancellation(t *testing.T) {
	db, _ := odooTestDB(t)
	seedOdooMinutes(t, db, 1, odooTestResult)
	app := &App{DB: db, Config: &Config{}}
	app.runOdooSync(context.Background()) // must return immediately without network
	j := mustClaimOdoo(t, db)
	if j.Attempts != 1 {
		t.Fatal("disabled worker touched queue")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	p := &odooSnapshot{SchemaVersion: 1, SourceID: "meet-test", ExternalID: "1", Revision: 1, Generation: 1, Operation: "delete"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, retry := odooTestClient(server).send(ctx, p)
	if code != "transport_error" || !retry {
		t.Fatal("cancelled request not recoverable")
	}
}
