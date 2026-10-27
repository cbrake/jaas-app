package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func odooOwnerMapTestConfig() *Config {
	return &Config{OdooSyncEnabled: true, OdooBaseURL: "https://odoo.example.test", OdooDatabase: "test-db", OdooSourceID: "meet-test-source"}
}

func odooOwnerMapTestDocument() odooOwnerMapDocument {
	return odooOwnerMapDocument{
		Version: 1, OdooBaseURL: "https://odoo.example.test", OdooDatabase: "test-db", OdooSourceID: "meet-test-source",
		Owners: []odooOwnerMapEntry{
			{MeetEmail: "owner@example.test", OdooUserID: 101, OdooEmail: "existing-profile@example.test"},
			{MeetEmail: "second@example.test", OdooUserID: 102, OdooEmail: "second@example.test"},
		},
	}
}

func odooOwnerMapTestBytes(t *testing.T, doc odooOwnerMapDocument) []byte {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func odooOwnerMapTestLoad(t *testing.T, cfg *Config, doc odooOwnerMapDocument) {
	t.Helper()
	m, err := parseOdooOwnerMap(odooOwnerMapTestBytes(t, doc), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.odooOwnerMap = m
}

func TestOdooOwnerMapStrictValidation(t *testing.T) {
	valid := string(odooOwnerMapTestBytes(t, odooOwnerMapTestDocument()))
	cases := map[string]string{
		"duplicate version":      strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"duplicate escaped key":  strings.Replace(valid, `"version":1`, `"version":1,"\u0076ersion":1`, 1),
		"unknown field":          strings.Replace(valid, `"version":1`, `"version":1,"extra":true`, 1),
		"wrong key case":         strings.Replace(valid, `"version":1`, `"Version":1`, 1),
		"missing field":          strings.Replace(valid, `"version":1,`, "", 1),
		"null field":             strings.Replace(valid, `"version":1`, `"version":null`, 1),
		"unsupported version":    strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"trailing object":        valid + `{}`,
		"trailing garbage":       valid + `bad`,
		"owner duplicate key":    strings.Replace(valid, `"odoo_user_id":101`, `"odoo_user_id":101,"odoo_user_id":101`, 1),
		"owner unknown key":      strings.Replace(valid, `"odoo_user_id":101`, `"odoo_user_id":101,"name":"someone"`, 1),
		"owner wrong key case":   strings.Replace(valid, `"odoo_user_id":101`, `"Odoo_user_id":101`, 1),
		"owner invalid id":       strings.Replace(valid, `"odoo_user_id":101`, `"odoo_user_id":0`, 1),
		"owner decimal id":       strings.Replace(valid, `"odoo_user_id":101`, `"odoo_user_id":101.0`, 1),
		"owner null email":       strings.Replace(valid, `"meet_email":"owner@example.test"`, `"meet_email":null`, 1),
		"owner display name":     strings.Replace(valid, `"owner@example.test"`, `"Name <owner@example.test>"`, 1),
		"owner duplicate source": strings.Replace(valid, `"meet_email":"second@example.test"`, `"meet_email":"OWNER@example.test"`, 1),
		"owner duplicate target": strings.Replace(valid, `"odoo_email":"second@example.test"`, `"odoo_email":"EXISTING-PROFILE@example.test"`, 1),
		"owner duplicate id":     strings.Replace(valid, `"odoo_user_id":102`, `"odoo_user_id":101`, 1),
		"wrong base":             strings.Replace(valid, "odoo.example.test", "other.example.test", 1),
		"wrong database":         strings.Replace(valid, "test-db", "other-db", 1),
		"wrong source":           strings.Replace(valid, "meet-test-source", "other-source", 1),
		"oversize":               strings.Repeat(" ", odooOwnerMapMaxBytes) + valid,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOdooOwnerMap([]byte(data), odooOwnerMapTestConfig()); err == nil || err.Error() != "ODOO_OWNER_MAP_FILE is invalid" {
				t.Fatal("invalid correspondence accepted or error disclosed input")
			}
		})
	}
	for _, owners := range [][]odooOwnerMapEntry{nil, {}, make([]odooOwnerMapEntry, 129)} {
		doc := odooOwnerMapTestDocument()
		doc.Owners = owners
		if _, err := parseOdooOwnerMap(odooOwnerMapTestBytes(t, doc), odooOwnerMapTestConfig()); err == nil {
			t.Fatal("invalid owner count accepted")
		}
	}
}

func TestOdooOwnerMapCanonicalDigestAndProjection(t *testing.T) {
	cfg := odooOwnerMapTestConfig()
	doc := odooOwnerMapTestDocument()
	odooOwnerMapTestLoad(t, cfg, doc)
	originalDigest := cfg.odooOwnerMapDigest()
	doc.Owners[0], doc.Owners[1] = doc.Owners[1], doc.Owners[0]
	doc.Owners[1].MeetEmail = " OWNER@EXAMPLE.TEST "
	doc.Owners[1].OdooEmail = " EXISTING-PROFILE@EXAMPLE.TEST "
	doc.OdooBaseURL += "/"
	pretty, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := parseOdooOwnerMap(pretty, cfg)
	if err != nil || reordered.digest != originalDigest {
		t.Fatal("formatting, case or ordering changed the correspondence binding")
	}
	p := &odooSnapshot{Operation: "upsert", OwnerEmail: "owner@example.test", Summary: "private summary", Revision: 7, Generation: 2}
	if err := cfg.mapOdooSnapshotOwner(p); err != nil || p.OwnerEmail != "existing-profile@example.test" || p.Summary != "private summary" || p.Revision != 7 || p.Generation != 2 {
		t.Fatal("projection must change only the outbound owner address")
	}
	if err := cfg.mapOdooSnapshotOwner(&odooSnapshot{Operation: "upsert", OwnerEmail: "unmapped@example.test"}); err == nil || err.Error() != "owner_mapping_missing" {
		t.Fatal("unmapped owner did not fail closed")
	}
	if err := cfg.mapOdooSnapshotOwner(&odooSnapshot{Operation: "delete"}); err != nil {
		t.Fatal("tombstone depended on owner correspondence")
	}
	plain := &odooSnapshot{Operation: "upsert", OwnerEmail: "owner@example.test"}
	if err := (&Config{}).mapOdooSnapshotOwner(plain); err != nil || plain.OwnerEmail != "owner@example.test" {
		t.Fatal("legacy direct mode changed")
	}
}

func TestOdooOwnerMapPrivateFileAndDisabledConfig(t *testing.T) {
	cfg := odooOwnerMapTestConfig()
	dir := t.TempDir()
	path := filepath.Join(dir, "owner-map.json")
	data := odooOwnerMapTestBytes(t, odooOwnerMapTestDocument())
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0600, 0400, 0440} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := loadOdooOwnerMap(path, cfg); err != nil {
			t.Fatalf("private file mode %o rejected: %v", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0644, 0660, 0700} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := loadOdooOwnerMap(path, cfg); err == nil {
			t.Fatalf("unsafe file mode %o accepted", mode)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{link, fifo, dir, filepath.Join(dir, "missing")} {
		if _, err := loadOdooOwnerMap(invalid, cfg); err == nil || err.Error() != "ODOO_OWNER_MAP_FILE is invalid" {
			t.Fatal("invalid file accepted or error disclosed private path")
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", odooOwnerMapMaxBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOdooOwnerMap(path, cfg); err == nil {
		t.Fatal("oversized private file accepted")
	}
	t.Setenv("ODOO_SYNC_ENABLED", "false")
	t.Setenv("ODOO_OWNER_MAP_FILE", fifo)
	if err := loadOdooSyncConfig(&Config{}); err != nil {
		t.Fatal("disabled sync accessed the correspondence file")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ODOO_SYNC_ENABLED", "true")
	t.Setenv("ODOO_BASE_URL", cfg.OdooBaseURL)
	t.Setenv("ODOO_DATABASE", cfg.OdooDatabase)
	t.Setenv("ODOO_SOURCE_ID", cfg.OdooSourceID)
	t.Setenv("ODOO_API_KEY_FILE", "")
	t.Setenv("ODOO_API_KEY", "synthetic-key")
	t.Setenv("ODOO_OWNER_MAP_FILE", path)
	loaded := &Config{}
	if err := loadOdooSyncConfig(loaded); err != nil || loaded.odooOwnerMap == nil {
		t.Fatal("enabled configuration did not load correspondence")
	}
}

func TestOdooOwnerMapBindingRejectsAllSemanticChanges(t *testing.T) {
	db, _ := odooTestDB(t)
	cfg := odooOwnerMapTestConfig()
	doc := odooOwnerMapTestDocument()
	odooOwnerMapTestLoad(t, cfg, doc)
	if err := db.bindOdooSyncIdentity(cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.bindOdooSyncIdentity(cfg); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*odooOwnerMapDocument){
		func(d *odooOwnerMapDocument) { d.Owners[0].MeetEmail = "new@example.test" },
		func(d *odooOwnerMapDocument) { d.Owners[0].OdooEmail = "new@example.test" },
		func(d *odooOwnerMapDocument) { d.Owners[0].OdooUserID = 999 },
		func(d *odooOwnerMapDocument) { d.Owners = d.Owners[:1] },
	} {
		changed := odooOwnerMapTestDocument()
		change(&changed)
		odooOwnerMapTestLoad(t, cfg, changed)
		if err := db.bindOdooSyncIdentity(cfg); err == nil || strings.Contains(err.Error(), "example.test") {
			t.Fatal("bound correspondence changed or error disclosed an address")
		}
	}
	cfg.odooOwnerMap = nil
	if err := db.bindOdooSyncIdentity(cfg); err == nil {
		t.Fatal("removing the bound correspondence accepted")
	}
	var digest string
	if err := db.conn.QueryRow("SELECT owner_map_digest FROM odoo_sync_identity WHERE id=1").Scan(&digest); err != nil || !strings.HasPrefix(digest, "map-v1:") {
		t.Fatal("failed bind overwrote original digest")
	}
}

func TestOdooOwnerMapV7UpgradePreservesQueueAndLegacyBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:7] {
		if _, err := conn.Exec(migration); err != nil {
			t.Fatal(err)
		}
	}
	_, err = conn.Exec(`INSERT INTO odoo_sync_identity(id,source_id,base_url,database_name)
 VALUES(1,'meet-test-source','https://odoo.example.test','test-db');
 INSERT INTO odoo_sync_outbox(minutes_id,revision,generation,operation,status,attempts,error_code)
 VALUES(1,9,2,'upsert','pending',0,''); PRAGMA user_version=7;`)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	db, err := InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := odooOwnerMapTestConfig()
	odooOwnerMapTestLoad(t, cfg, odooOwnerMapTestDocument())
	if err := db.bindOdooSyncIdentity(cfg); err == nil {
		t.Fatal("v7 identity gained a mapping that could alter a retried revision")
	}
	cfg.odooOwnerMap = nil
	if err := db.bindOdooSyncIdentity(cfg); err != nil {
		t.Fatal(err)
	}
	var revision, generation, attempts, version int
	var status, digest string
	if err := db.conn.QueryRow("SELECT revision,generation,status,attempts FROM odoo_sync_outbox WHERE minutes_id=1").Scan(&revision, &generation, &status, &attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.conn.QueryRow("SELECT owner_map_digest FROM odoo_sync_identity WHERE id=1").Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if err := db.conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if revision != 9 || generation != 2 || status != "pending" || attempts != 0 || digest != "direct-v1" || version != 8 {
		t.Fatal("v8 migration changed legacy queue or binding")
	}
}

func TestOdooOwnerMapWorkerKeepsMeetOwnerAndReplayPayload(t *testing.T) {
	db, now := odooTestDB(t)
	seedOdooMinutes(t, db, 1, odooTestResult)
	cfg := odooOwnerMapTestConfig()
	odooOwnerMapTestLoad(t, cfg, odooOwnerMapTestDocument())
	if err := db.bindOdooSyncIdentity(cfg); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Payload odooSnapshot `json:"payload"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Payload.OwnerEmail != "existing-profile@example.test" || body.Payload.Revision != 1 {
			t.Error("mapped delivery or retry changed identity/revision")
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"source_id": body.Payload.SourceID, "external_id": body.Payload.ExternalID, "revision": body.Payload.Revision, "status": "duplicate"})
	}))
	defer server.Close()
	app := &App{DB: db, Config: cfg}
	client := odooTestClient(server)
	app.processOdooSync(context.Background(), client)
	*now = now.Add(time.Minute)
	app.processOdooSync(context.Background(), client)
	var meetOwner, state string
	if err := db.conn.QueryRow("SELECT owner_email FROM rooms WHERE slug='customer-review'").Scan(&meetOwner); err != nil {
		t.Fatal(err)
	}
	if err := db.conn.QueryRow("SELECT status FROM odoo_sync_outbox WHERE minutes_id=1").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || meetOwner != "owner@example.test" || state != "sent" {
		t.Fatal("delivery changed Meet identity or failed to retry")
	}
}

func TestOdooOwnerMapUnmappedOwnerFailsButCancellationSends(t *testing.T) {
	db, _ := odooTestDB(t)
	seedOdooMinutes(t, db, 1, odooTestResult)
	cfg := odooOwnerMapTestConfig()
	doc := odooOwnerMapTestDocument()
	doc.Owners = doc.Owners[1:]
	odooOwnerMapTestLoad(t, cfg, doc)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Payload map[string]any `json:"payload"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Payload["operation"] != "delete" || len(body.Payload) != 6 {
			t.Error("unmapped owner sent data or cancellation leaked content")
		}
		json.NewEncoder(w).Encode(map[string]any{"source_id": body.Payload["source_id"], "external_id": body.Payload["external_id"], "revision": body.Payload["revision"], "status": "applied"})
	}))
	defer server.Close()
	app := &App{DB: db, Config: cfg}
	client := odooTestClient(server)
	app.processOdooSync(context.Background(), client)
	var code, status string
	if err := db.conn.QueryRow("SELECT error_code,status FROM odoo_sync_outbox WHERE minutes_id=1").Scan(&code, &status); err != nil {
		t.Fatal(err)
	}
	if code != "owner_mapping_missing" || status != "failed" || calls.Load() != 0 {
		t.Fatal("unmapped owner was sent or did not fail with sanitized code")
	}
	if err := db.CancelMinutes(1); err != nil {
		t.Fatal(err)
	}
	app.processOdooSync(context.Background(), client)
	if calls.Load() != 1 {
		t.Fatal("cancellation was blocked by missing owner correspondence")
	}
}
