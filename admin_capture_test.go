package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSessionSearchOwnerDateAndLiteralFilter(t *testing.T) {
	e := newTestEnv(t)
	db := e.app.DB
	for _, row := range []struct{ slug, owner string }{{"alpha-room", "a@example.test"}, {"beta-room", "b@example.test"}} {
		if err := db.CreateRoom(row.slug, "x", row.owner); err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.Exec("INSERT INTO capture_runs(room_slug,started_at,last_activity_at,status,client_name,project_name) VALUES(?,?,?,'open','Cliente A','Proyecto 100%')", row.slug, e.now.Unix(), e.now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	f := sessionFilter{From: "2026-10-14", To: "2026-10-14", Project: "100%", Page: 1}
	rows, more, err := db.searchSessions(f, "a@example.test", false, e.app.Config.DisplayTimezone)
	if err != nil || more || len(rows) != 1 || rows[0].Room != "alpha-room" {
		t.Fatalf("owner filter: %+v %v", rows, err)
	}
	f.From = "2026-10-15"
	if _, _, err = db.searchSessions(f, "a@example.test", false, e.app.Config.DisplayTimezone); err == nil {
		t.Fatal("invalid dates accepted")
	}
	f.From = ""
	f.To = "2026-10-13"
	rows, _, err = db.searchSessions(f, "", true, e.app.Config.DisplayTimezone)
	if err != nil || len(rows) != 0 {
		t.Fatal("local day boundary")
	}
}

func TestPrivateVisualizationAndAssetsRequireAuthentication(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/visualizations", "/visualizations/", "/visualizations/assets/app.js", "/visualizations/assets/app.css", "/admin/captures"} {
		rr := e.browser().do(http.MethodGet, path, nil)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("anonymous %s: %d", path, rr.Code)
		}
	}
	b, _ := e.loginAdmin()
	for _, path := range []string{"/visualizations", "/visualizations/assets/app.js", "/visualizations/assets/app.css"} {
		rr := b.do(http.MethodGet, path, nil)
		if rr.Code != 200 || rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("private %s: %d", path, rr.Code)
		}
		if strings.Contains(rr.Header().Get("Content-Security-Policy"), "script-src 'self' 'unsafe-inline'") {
			t.Fatal("CSP weakened")
		}
	}
	page := b.do(http.MethodGet, "/visualizations", nil).Body.String()
	if strings.Contains(page, "<script>") || !strings.Contains(page, "Ejemplo simulado") {
		t.Fatal("visualization contract")
	}
}

func TestCancelledMinutesCannotRunCompleteRetryOrSend(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "cancel-room")
	db := e.app.DB
	db.SaveTranscription("cancel-room", "s1", testTranscriptJSON)
	db.EnqueueMinutes("cancel-room", "s1", "", e.now, e.now)
	ms, _ := db.ListMinutes("cancel-room")
	id := ms[0].ID
	if rr := b.do(http.MethodPost, "/admin/minutes/"+itoa(id)+"/cancel", url.Values{"csrf_token": {csrf}}); rr.Code != 303 {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body.String())
	}
	if e.app.processOne(context.Background()) || e.llm.calls != 0 {
		t.Fatal("cancelled job called LLM")
	}
	if err := db.CompleteMinutes(id, validMinutesJSON, "model", []string{"host@example.test"}); err == nil {
		t.Fatal("late completion resurrected cancellation")
	}
	if ok, _ := db.RequeueMinutes(id); ok {
		t.Fatal("cancelled retry allowed")
	}
	if _, err := db.QueueDeliveries(id, []Recipient{{Email: "x@example.test"}}, "admin@example.test"); err == nil {
		t.Fatal("cancelled delivery allowed")
	}
	m, _ := db.GetMinutes(id)
	if m.Status != "cancelled" {
		t.Fatal("cancelled state hidden")
	}
}

func TestCaptureActionsEnforceOwnerAndCSRF(t *testing.T) {
	e := newTestEnv(t)
	db := e.app.DB
	db.CreateRoom("owned-room", "x", "owner@example.test")
	res, err := db.conn.Exec("INSERT INTO capture_runs(room_slug,started_at,last_activity_at,status)VALUES('owned-room',?,?,'open')", e.now.Unix(), e.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	r := httptest.NewRequest("POST", "/admin/captures/"+itoa(id)+"/cancel", nil)
	r.SetPathValue("id", itoa(id))
	r = r.WithContext(contextWithAdmin(r, &AdminSession{Email: "stranger@example.test"}))
	w := httptest.NewRecorder()
	e.app.handleCancelCapture(w, r)
	if w.Code != 404 {
		t.Fatalf("cross-owner: %d", w.Code)
	}
	b, _ := e.loginAdmin()
	if rr := b.do("POST", "/admin/captures/"+itoa(id)+"/cancel", url.Values{}); rr.Code != 403 {
		t.Fatalf("CSRF: %d", rr.Code)
	}
}

func TestQualityGateAndWindowEvidence(t *testing.T) {
	doc := `{"source":"browser-capture","messages":[{"name":"Sala","content":"Buenas tardes","timestamp":1760464800000,"time_precision":"window","speaker_verified":false}]}`
	if transcriptQuality(doc) == nil {
		t.Fatal("greeting should require review")
	}
	if !unmeasuredTranscript(doc) {
		t.Fatal("window metric false precision")
	}
	var result MinutesResult
	if err := json.Unmarshal([]byte(validMinutesJSON), &result); err != nil {
		t.Fatal(err)
	}
	result.ActionItems = nil
	result.Sentiment.BySpeaker = nil
	result.Sentiment.Moments = nil
	if err := validateEvidence(&result, testTranscriptJSON); err != nil {
		t.Fatal(err)
	}
	result.ActionItems = []MinutesActionItem{{Evidence: "Ana: esto jamás se dijo", Task: "Inventada"}}
	if err := validateEvidence(&result, testTranscriptJSON); err == nil {
		t.Fatal("invented evidence accepted")
	}
}

func TestLocalDateBounds(t *testing.T) {
	loc, _ := time.LoadLocation("America/Mexico_City")
	start, _ := dateBound("2026-10-25", loc, false)
	end, _ := dateBound("2026-10-25", loc, true)
	if time.Unix(start, 0).UTC().Hour() != 6 || end-start != 86400 {
		t.Fatal("CDMX inclusive day incorrect")
	}
}

type brokenMinutesBody struct{}

func (brokenMinutesBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenMinutesBody) Close() error             { return nil }

type interruptedMinutesTransport struct{ calls int }

func (t *interruptedMinutesTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": []string{"req-interrupted"}}, Body: brokenMinutesBody{}, Request: r}, nil
}

func TestMinutesUncertainResponsesDoNotRetry(t *testing.T) {
	for _, body := range []string{`{`, `{"status":"completed","output":[]}`, `{"summary":"missing schema"}`} {
		e := newTestEnv(t)
		e.app.DB.CreateRoom("quality-room", "x", "")
		e.app.DB.SaveTranscription("quality-room", "s1", testTranscriptJSON)
		e.app.DB.EnqueueMinutes("quality-room", "s1", "", e.now, e.now)
		if strings.Contains(body, "summary") {
			e.llm.out = []byte(body)
		} else {
			rt := &fakeTransport{status: 200, body: body}
			e.app.LLM = &openAIClient{BaseURL: "https://api.example.test/v1", Key: "fixture", Model: "fixture", HTTP: &http.Client{Transport: rt}}
		}
		e.app.processOne(context.Background())
		ms, _ := e.app.DB.ListMinutes("quality-room")
		if ms[0].Status != "failed" || ms[0].Attempts != 1 {
			t.Fatalf("uncertain response repeated: %+v", ms[0])
		}
		e.advance(time.Hour)
		if e.app.processOne(context.Background()) {
			t.Fatal("automatic retry")
		}
	}
	rt := &interruptedMinutesTransport{}
	c := &openAIClient{BaseURL: "https://api.example.test/v1", Key: "fixture", Model: "fixture", HTTP: &http.Client{Transport: rt}}
	_, _, err := c.Generate(context.Background(), MinutesInput{Timezone: time.UTC})
	if !isPermanent(err) || rt.calls != 1 {
		t.Fatalf("interrupted response should require review: %v", err)
	}
}

func TestWindowTimestampRenderingRemainsExplicit(t *testing.T) {
	doc := `{"messages":[{"name":"Sala","content":"Texto de varios minutos","timestamp":1760464800000,"time_precision":"window"},{"name":"Ana","content":"Intervención breve","timestamp":1760464810000} ]}`
	text, n := renderTranscript(doc, time.UTC)
	if n != 2 || !strings.Contains(text, "Ventana desde") || !strings.Contains(text, "sin tiempos por intervención") || !strings.Contains(text, "] Ana: Intervención breve") {
		t.Fatal(text)
	}
}

func TestPilotDoesNotQueueJaaSTranscription(t *testing.T) {
	e := newTestEnv(t)
	e.app.Config.TranscribeModel = "gpt-transcribe"
	e.app.DB.CreateRoom("single-asr", "x", "")
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "pilot-event", "external", "single-asr", testLink))
	drain(e)
	if e.llm.calls != 0 || e.rt.requests != 0 {
		t.Fatal("second transcription pipeline started")
	}
}

func TestJoinNoticeUsesConfiguredAudioRetention(t *testing.T) {
	e := newTestEnv(t)
	e.app.Config.AudioRetentionDays = 17
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "notice-room")
	page := e.browser().do(http.MethodGet, "/m/notice-room", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "17 días") || strings.Contains(page.Body.String(), "después se borra") {
		t.Fatalf("retention notice: %d", page.Code)
	}
}
