package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func drain(e *testEnv) {
	ctx := context.Background()
	for e.app.processCapture(ctx) || e.app.processOne(ctx) || e.app.processHostNotification(ctx) || e.app.processDelivery(ctx) {
	}
}

func TestWorkerProducesOneDraftPerSession(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "acme-project-kickoff-2026")
	slug := "acme-project-kickoff-2026"

	// JaaS retries plus a second delivery with another key for the same session.
	ev := eventJSON("TRANSCRIPTION_UPLOADED", "k1", "sess-1", slug, testLink)
	e.signedWebhook(ev)
	e.signedWebhook(ev)
	drain(e)
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k2", "sess-1", slug, testLink))
	drain(e)

	if n, _ := e.app.DB.CountMinutesForSession(slug, "sess-1"); n != 1 {
		t.Fatalf("minutes rows for session: %d", n)
	}
	if e.llm.calls != 1 || e.rt.requests != 1 {
		t.Fatalf("LLM calls %d, downloads %d; want 1 and 1", e.llm.calls, e.rt.requests)
	}
	ms, _ := e.app.DB.ListMinutes(slug)
	m := ms[0]
	if m.Status != "ready" || m.Model != "test-model" {
		t.Fatalf("status %s model %s err %s", m.Status, m.Model, m.Error)
	}
	var src *string
	e.app.DB.conn.QueryRow("SELECT source_url FROM minutes WHERE id = ?", m.ID).Scan(&src)
	if src != nil {
		t.Fatal("pre-authenticated link kept after download")
	}
	if !strings.Contains(e.llm.last.Transcript, "Ana: Pablo sends the proposal on Friday") {
		t.Fatalf("transcript sent to model: %q", e.llm.last.Transcript)
	}

	// Draft is viewable and downloadable; nothing is sent anywhere.
	page := b.do(http.MethodGet, "/admin/minutes/"+itoa(m.ID), nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Automatically generated draft") || !strings.Contains(page.Body.String(), "Send proposal") {
		t.Fatalf("minutes page: %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "Upgrade to version 18") || !strings.Contains(page.Body.String(), "Sentiment analysis") {
		t.Fatal("key points or sentiment missing from the minutes page")
	}
	md := b.do(http.MethodGet, "/admin/minutes/"+itoa(m.ID)+"/download", nil)
	if !strings.Contains(md.Body.String(), "## Key points") || !strings.Contains(md.Body.String(), "**positive.**") {
		t.Fatalf("markdown sections: %s", md.Body.String())
	}
	if !strings.Contains(md.Header().Get("Content-Disposition"), ".md") || !strings.Contains(md.Body.String(), "| Send proposal | Pablo | 2026-10-16 | high |") {
		t.Fatalf("markdown: %s", md.Body.String())
	}
	js := b.do(http.MethodGet, "/admin/minutes/"+itoa(m.ID)+"/download?format=json", nil)
	var doc struct {
		Status  string        `json:"status"`
		Minutes MinutesResult `json:"minutes"`
	}
	if err := json.Unmarshal(js.Body.Bytes(), &doc); err != nil || doc.Status != "draft" || len(doc.Minutes.ActionItems) != 2 {
		t.Fatalf("json download: %v %+v", err, doc)
	}
	// Anonymous users cannot read minutes.
	if rr := e.browser().do(http.MethodGet, "/admin/minutes/"+itoa(m.ID), nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("anonymous minutes access: %d", rr.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestAIMinutesAreOptional(t *testing.T) {
	e := newTestEnv(t)
	e.app.LLM, e.app.Transcriber = nil, nil // no OPENAI_API_KEY
	fm := &fakeMailer{}
	e.app.Mailer = fm
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "test-room")
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink))
	drain(e)
	if ts, _ := e.app.DB.ListTranscriptions("test-room"); len(ts) != 1 {
		t.Fatalf("JaaS transcript not stored: %d", len(ts))
	}
	ms, _ := e.app.DB.ListMinutes("test-room")
	if len(ms) != 1 || ms[0].Status != "failed" || !strings.Contains(ms[0].Error, "AI minutes are disabled") || e.llm.calls != 0 {
		t.Fatalf("minutes without a model: %+v calls=%d", ms, e.llm.calls)
	}
	if len(fm.sent) != 0 {
		t.Fatal("host notified about minutes that do not exist")
	}
	page := b.do(http.MethodGet, "/admin", nil).Body.String()
	if strings.Contains(page, "Minutes (drafts)") || !strings.Contains(page, "Transcripts") || !strings.Contains(page, "Transcribe (JaaS)") {
		t.Fatal("dashboard offers AI features that are off")
	}
}

func TestWorkerRetriesThenFails(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	e.llm.out = []byte(`{"summary": "without the other keys"}`)
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink))

	for attempt := 1; attempt <= e.app.Config.WorkerMaxAttempts; attempt++ {
		if !e.app.processOne(context.Background()) {
			t.Fatalf("attempt %d: nothing claimed", attempt)
		}
		ms, _ := e.app.DB.ListMinutes("test-room")
		want := "queued"
		if attempt == e.app.Config.WorkerMaxAttempts {
			want = "failed"
		}
		if ms[0].Status != want || !strings.Contains(ms[0].Error, "invalid schema") {
			t.Fatalf("attempt %d: status %s error %q", attempt, ms[0].Status, ms[0].Error)
		}
		// Not due yet: backoff is honoured.
		if want == "queued" && e.app.processOne(context.Background()) {
			t.Fatalf("attempt %d: retried before backoff", attempt)
		}
		e.advance(4 * time.Hour)
	}
	// Admin retry after fixing the cause: transcript is stored, no new download.
	e.llm.out = []byte(validMinutesJSON)
	ms, _ := e.app.DB.ListMinutes("test-room")
	if ok, _ := e.app.DB.RequeueMinutes(ms[0].ID); !ok {
		t.Fatal("requeue refused")
	}
	drain(e)
	ms, _ = e.app.DB.ListMinutes("test-room")
	if ms[0].Status != "ready" || e.rt.requests != 1 {
		t.Fatalf("after retry: %s, downloads %d", ms[0].Status, e.rt.requests)
	}
}

func TestWorkerPermanentDownloadError(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	e.rt.status = http.StatusForbidden
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink))
	drain(e)
	ms, _ := e.app.DB.ListMinutes("test-room")
	if ms[0].Status != "failed" || ms[0].Error != "download: HTTP 403" || ms[0].Attempts != 1 {
		t.Fatalf("status %s attempts %d error %q", ms[0].Status, ms[0].Attempts, ms[0].Error)
	}
	// A later webhook with a fresh link for the same session requeues it.
	e.rt.status = http.StatusOK
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k2", "s1", "test-room", testLink))
	drain(e)
	ms, _ = e.app.DB.ListMinutes("test-room")
	if len(ms) != 1 || ms[0].Status != "ready" {
		t.Fatalf("after new link: %+v", ms)
	}
}

func TestWorkerExpiredLink(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink))
	e.advance(25 * time.Hour)
	drain(e)
	ms, _ := e.app.DB.ListMinutes("test-room")
	if ms[0].Status != "failed" || !strings.Contains(ms[0].Error, "expired") || e.rt.requests != 0 {
		t.Fatalf("expired link: %s %q downloads %d", ms[0].Status, ms[0].Error, e.rt.requests)
	}
}

func TestWorkerLeaseRecoversAfterCrash(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink))
	if _, err := e.app.DB.ClaimNextMinutes(15 * time.Minute); err != nil { // claimed, then the process dies
		t.Fatal(err)
	}
	if e.app.processOne(context.Background()) {
		t.Fatal("job claimed twice while leased")
	}
	e.advance(16 * time.Minute)
	drain(e)
	ms, _ := e.app.DB.ListMinutes("test-room")
	if ms[0].Status != "ready" || ms[0].Attempts != 2 {
		t.Fatalf("after lease expiry: %s attempts %d", ms[0].Status, ms[0].Attempts)
	}
}

func TestValidateMinutes(t *testing.T) {
	if _, err := validateMinutes([]byte(validMinutesJSON)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	mutate := func(f func(m map[string]any)) []byte {
		var m map[string]any
		json.Unmarshal([]byte(validMinutesJSON), &m)
		f(m)
		b, _ := json.Marshal(m)
		return b
	}
	item := func(m map[string]any) map[string]any { return m["action_items"].([]any)[0].(map[string]any) }
	bad := map[string][]byte{
		"not json":           []byte("hello"),
		"missing risks":      mutate(func(m map[string]any) { delete(m, "risks") }),
		"extra key":          mutate(func(m map[string]any) { m["send_email_to"] = "x@y.z" }),
		"empty summary":      mutate(func(m map[string]any) { m["summary"] = " " }),
		"empty language":     mutate(func(m map[string]any) { m["language"] = " " }),
		"bad date":           mutate(func(m map[string]any) { item(m)["due_date"] = "friday" }),
		"bad confidence":     mutate(func(m map[string]any) { item(m)["confidence"] = "certain" }),
		"missing evidence":   mutate(func(m map[string]any) { delete(item(m), "evidence") }),
		"null list":          mutate(func(m map[string]any) { m["risks"] = nil }),
		"empty task":         mutate(func(m map[string]any) { item(m)["task"] = "" }),
		"owner wrong type":   mutate(func(m map[string]any) { item(m)["owner"] = 7 }),
		"extra item key":     mutate(func(m map[string]any) { item(m)["assign_in_crm"] = true }),
		"missing key_points": mutate(func(m map[string]any) { delete(m, "key_points") }),
		"bad sentiment":      mutate(func(m map[string]any) { m["sentiment"].(map[string]any)["overall"] = "happy" }),
		"sentiment no expl":  mutate(func(m map[string]any) { delete(m["sentiment"].(map[string]any), "explanation") }),
	}
	for name, raw := range bad {
		if _, err := validateMinutes(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOpenAIResponsesClient(t *testing.T) {
	var got map[string]any
	var auth string
	mode := "ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		switch mode {
		case "429":
			http.Error(w, `{"error":{"message":"echo of the transcript"}}`, http.StatusTooManyRequests)
		case "401":
			http.Error(w, "nope", http.StatusUnauthorized)
		case "refusal":
			w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`))
		case "incomplete":
			w.Write([]byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`))
		default:
			out, _ := json.Marshal(validMinutesJSON)
			w.Write([]byte(`{"status":"completed","model":"gpt-test-2026","output":[{"type":"reasoning","summary":[]},{"type":"message","content":[{"type":"output_text","text":` + string(out) + `}]}]}`))
		}
	}))
	defer srv.Close()
	c := &openAIClient{BaseURL: srv.URL + "/v1", Key: "sk-test-key", Model: "gpt-test", HTTP: srv.Client()}
	in := MinutesInput{RoomSlug: "sala", MeetingAt: time.Now(), Transcript: "Ana: hello\n"}

	raw, model, err := c.Generate(context.Background(), in)
	if err != nil || model != "gpt-test-2026" {
		t.Fatalf("generate: %v %s", err, model)
	}
	if _, err := validateMinutes(raw); err != nil {
		t.Fatalf("output: %v", err)
	}
	if auth != "Bearer sk-test-key" || got["model"] != "gpt-test" || got["store"] != false {
		t.Fatalf("request: auth=%q model=%v store=%v", auth, got["model"], got["store"])
	}
	format := got["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true || format["schema"] == nil {
		t.Fatalf("structured output not requested: %v", format)
	}

	for m, wantPermanent := range map[string]bool{"429": false, "401": true, "refusal": true, "incomplete": true} {
		mode = m
		_, _, err := c.Generate(context.Background(), in)
		if err == nil || isPermanent(err) != wantPermanent {
			t.Errorf("%s: err=%v permanent=%v", m, err, isPermanent(err))
		}
		if err != nil && strings.Contains(err.Error(), "echo of the transcript") {
			t.Errorf("%s: provider body leaked into error", m)
		}
	}
	var hs *httpStatusError
	mode = "429"
	if _, _, err := c.Generate(context.Background(), in); !errors.As(err, &hs) || hs.Code != 429 {
		t.Fatalf("429 not typed: %v", err)
	}
}

func TestRetentionPurge(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink))
	e.signedWebhook(eventJSON("RECORDING_UPLOADED", "r1", "s1", "test-room", testLink))
	drain(e)

	e.advance(25 * time.Hour)
	res, err := e.app.DB.PurgeExpired(Retention{Days: 90}, Retention{Days: 365})
	if err != nil || res["recordings"] != 1 {
		t.Fatalf("recordings purge: %v %v", err, res)
	}
	e.advance(91 * 24 * time.Hour)
	res, _ = e.app.DB.PurgeExpired(Retention{Days: 90}, Retention{Days: 365})
	if res["transcriptions"] != 1 || res["minutes"] != 0 || res["webhook_events"] != 2 {
		t.Fatalf("90-day purge: %v", res)
	}
	e.advance(300 * 24 * time.Hour)
	res, _ = e.app.DB.PurgeExpired(Retention{Days: 90}, Retention{Days: 365})
	if res["minutes"] != 1 {
		t.Fatalf("365-day purge: %v", res)
	}
}

// TestNoSecretsInLogs runs the whole flow and greps the captured log.
func TestNoSecretsInLogs(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "test-room")
	g := e.browser()
	tok := hidden(t, g.do(http.MethodGet, "/m/test-room", nil).Body.String(), "form_token")
	g.do(http.MethodPost, "/m/test-room/join", url.Values{"form_token": {tok}, "display_name": {"Host"}, "email": {"host@client.example"}, "consent": {"on"}, "host_password": {"host-secret-pw"}})
	g.do(http.MethodGet, "/m/test-room", nil)
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink))
	e.signedWebhook(eventJSON("RECORDING_UPLOADED", "r1", "s1", "test-room", testLink))
	e.postWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k9", "s9", "test-room", testLink), "t=1,v1=forged")
	e.llm.err = errors.New("openai: HTTP 500")
	drain(e)

	logs := e.logs.String()
	if logs == "" {
		t.Fatal("no logs captured")
	}
	for _, secret := range []string{
		testSecret, "SECRET-PREAUTH-TOKEN", testAdminPW, "Nueva-Contrasena-Segura-2026", "host-secret-pw",
		"Pablo sends the proposal", "eyJ", "csrf_token", "form_token", "__Host-",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

func TestRetentionNineCalendarMonths(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	nine := Retention{Months: 9}
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink))
	drain(e)
	created := e.now

	// One day before nine calendar months: nothing goes.
	e.now = created.AddDate(0, 9, -1)
	res, err := e.app.DB.PurgeExpired(nine, nine)
	if err != nil || res["transcriptions"] != 0 || res["minutes"] != 0 {
		t.Fatalf("purged too early: %v %v", err, res)
	}
	// One day after nine calendar months: transcript and minutes go.
	e.now = created.AddDate(0, 9, 1)
	res, _ = e.app.DB.PurgeExpired(nine, nine)
	if res["transcriptions"] != 1 || res["minutes"] != 1 {
		t.Fatalf("nine-month purge: %v", res)
	}
}

func TestLoadRetentionFromEnv(t *testing.T) {
	t.Setenv("TRANSCRIPT_RETENTION_MONTHS", "9")
	t.Setenv("TRANSCRIPT_RETENTION_DAYS", "30")
	if r, err := loadRetention("TRANSCRIPT", 90); err != nil || r != (Retention{Months: 9}) {
		t.Fatalf("months must win over days: %+v %v", r, err)
	}
	t.Setenv("MINUTES_RETENTION_MONTHS", "")
	t.Setenv("MINUTES_RETENTION_DAYS", "")
	if r, _ := loadRetention("MINUTES", 365); r != (Retention{Days: 365}) || r.String() != "365 days" {
		t.Fatalf("default: %+v", r)
	}
	t.Setenv("MINUTES_RETENTION_MONTHS", "nueve")
	if _, err := loadRetention("MINUTES", 365); err == nil {
		t.Fatal("invalid value accepted")
	}
}
