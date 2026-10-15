package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testOrigin         = "https://meet.example.test"
	testAppID          = "vpaas-magic-cookie-0123456789abcdef"
	testSecret         = "whsec_test_0123456789abcdef"
	testAdminPW        = "Temporal-Admin-Password-2026"
	testLink           = "https://objectstorage.us-phoenix-1.oraclecloud.com/p/SECRET-PREAUTH-TOKEN/n/x/b/y/o/transcript.json"
	testTranscriptLine = "Ana: Pablo sends the proposal on Friday"
)

type fakeTransport struct {
	mu       sync.Mutex
	status   int
	body     string
	requests int
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

const validMinutesJSON = `{
  "summary": "The team reviewed the rollout project and agreed on the next steps.",
  "key_points": ["Upgrade to version 18", "Customer data still pending"],
  "sentiment": {"overall": "positive", "explanation": "The team agreed on the plan and the dates.",
    "by_speaker": [{"speaker": "Ana", "overall": "positive", "explanation": "Proposes and pins down dates.", "evidence": "Ana: Pablo sends the proposal on Friday"},
                   {"speaker": "Invented", "overall": "negative", "explanation": "Does not exist.", "evidence": "-"}],
    "timeline": [{"phase": "beginning", "overall": "neutral", "summary": "Review of open items."},
                 {"phase": "end", "overall": "positive", "summary": "Dates agreed."}],
    "moments": [{"time": "12:00:10", "speaker": "Pablo", "emotion": "agreement", "quote": "Agreed, I will send it on Friday"}],
    "caveats": "Tone of voice is not measured; short utterances."},
  "decisions": [{"decision": "Use version 18", "context": "Compatibility with current modules"}],
  "action_items": [{"task": "Send proposal", "owner": "Pablo", "due_date": "2026-10-16", "evidence": "Ana: Pablo sends the proposal on Friday", "confidence": "high"},
                   {"task": "Check inventory", "owner": null, "due_date": null, "evidence": "Ana: someone check the inventory", "confidence": "low"}],
  "risks": ["Delay in customer data"],
  "open_questions": ["Who approves the budget?"],
  "language": "en"
}`

const testTranscriptJSON = `{"conferenceName":"room","messages":[
 {"name":"Ana","content":"Pablo sends the proposal on Friday","timestamp":1760464800000},
 {"name":"Pablo","content":"Agreed, I will send it on Friday","timestamp":1760464810000}]}`

type testEnv struct {
	t    *testing.T
	app  *App
	h    http.Handler
	now  time.Time
	logs *bytes.Buffer
	llm  *stubLLM
	rt   *fakeTransport
}

type stubLLM struct {
	mu    sync.Mutex
	calls int
	out   []byte
	err   error
	last  MinutesInput
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := &testEnv{t: t, now: time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC), logs: &bytes.Buffer{}}
	clock := func() time.Time { return env.now }

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(env.logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.now = clock

	dialin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(dialin.Close)

	loc, _ := time.LoadLocation("America/Regina") // UTC-6, no DST
	proxies, _ := parseCIDRs([]string{"127.0.0.0/8"})
	cfg := &Config{
		PublicOrigin:         testOrigin,
		DBPath:               dbPath,
		JaaSAppID:            testAppID,
		JaaSKeyID:            testAppID + "/key1",
		JaaSKey:              testKey(t),
		WebhookSecret:        []byte(testSecret),
		WebhookTolerance:     5 * time.Minute,
		OpenAIModel:          "test-model",
		WorkerMaxAttempts:    3,
		MaxTranscriptSize:    1 << 20,
		MaxTranscriptChars:   100000,
		DownloadAllowedHosts: []string{"objectstorage.*.oraclecloud.com"},
		TrustedProxies:       proxies,
		TranscriptRetention:  Retention{Days: 90},
		MinutesRetention:     Retention{Days: 365},
		DisplayTimezone:      loc,
		SecureCookies:        true,
	}
	env.llm = &stubLLM{out: []byte(validMinutesJSON)}
	env.rt = &fakeTransport{status: http.StatusOK, body: testTranscriptJSON}
	app := &App{
		Config:          cfg,
		DB:              db,
		DialIn:          &DialInClient{BaseURL: dialin.URL},
		LLM:             env.llm,
		DownloadClient:  &http.Client{Transport: env.rt},
		loginLimiter:    newFailureLimiter(5, 15*time.Minute),
		hostLimiter:     newFailureLimiter(5, 15*time.Minute),
		hostRoomLimiter: newFailureLimiter(20, time.Hour),
		wake:            make(chan struct{}, 1),
		clock:           clock,
	}
	for _, l := range []*failureLimiter{app.loginLimiter, app.hostLimiter, app.hostRoomLimiter} {
		l.now = clock
	}
	if err := app.loadTemplates(); err != nil {
		t.Fatalf("templates: %v", err)
	}
	env.app = app
	env.h = app.Handler()
	return env
}

func (s *stubLLM) Generate(_ context.Context, in MinutesInput) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.last = in
	return s.out, "test-model", s.err
}

func (e *testEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

func signBody(secret, body string, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	return "t=" + ts + ",v1=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (e *testEnv) postWebhook(body, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, testOrigin+"/webhook/recording", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("X-Jaas-Signature", signature)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

func (e *testEnv) signedWebhook(body string) *httptest.ResponseRecorder {
	return e.postWebhook(body, signBody(testSecret, body, e.now))
}

func eventJSON(eventType, key, session, slug, link string) string {
	return fmt.Sprintf(`{"idempotencyKey":%q,"customerId":"c1","appId":%q,"eventType":%q,"sessionId":%q,"timestamp":1760464800000,"fqn":%q,"data":{"preAuthenticatedLink":%q}}`,
		key, testAppID, eventType, session, testAppID+"/"+slug, link)
}

// browser keeps cookies across requests the way a browser would for this host.
type browser struct {
	e       *testEnv
	cookies map[string]*http.Cookie
	origin  string
	ip      string
	headers map[string]string
}

func (e *testEnv) browser() *browser {
	return &browser{e: e, cookies: map[string]*http.Cookie{}, origin: testOrigin, ip: "203.0.113.10"}
}

func (b *browser) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, testOrigin+path, body)
	req.RemoteAddr = b.ip + ":40000"
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == http.MethodPost && b.origin != "" {
		req.Header.Set("Origin", b.origin)
	}
	for _, c := range b.cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	for k, v := range b.headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	b.e.h.ServeHTTP(rr, req)
	for _, c := range rr.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return rr
}

var hiddenRe = func(name string) *regexp.Regexp {
	return regexp.MustCompile(`name="` + name + `" value="([^"]+)"`)
}

func hidden(t *testing.T, html, name string) string {
	t.Helper()
	m := hiddenRe(name).FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("hidden field %s not found", name)
	}
	return m[1]
}

// loginAdmin bootstraps the admin, logs in and replaces the temporary
// password, returning a browser with a usable admin session and its CSRF.
func (e *testEnv) loginAdmin() (*browser, string) {
	e.t.Helper()
	if err := e.app.DB.SetAdminPassword(testAdminPW, true); err != nil {
		e.t.Fatal(err)
	}
	b := e.browser()
	page := b.do(http.MethodGet, "/admin/login", nil)
	rr := b.do(http.MethodPost, "/admin/login", url.Values{
		"form_token": {hidden(e.t, page.Body.String(), "form_token")}, "password": {testAdminPW},
	})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/password" {
		e.t.Fatalf("login: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	page = b.do(http.MethodGet, "/admin/password", nil)
	rr = b.do(http.MethodPost, "/admin/password", url.Values{
		"csrf_token":       {hidden(e.t, page.Body.String(), "csrf_token")},
		"current_password": {testAdminPW},
		"new_password":     {"Nueva-Contrasena-Segura-2026"},
		"confirm_password": {"Nueva-Contrasena-Segura-2026"},
	})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin" {
		e.t.Fatalf("password change: %d", rr.Code)
	}
	dash := b.do(http.MethodGet, "/admin", nil)
	if dash.Code != http.StatusOK {
		e.t.Fatalf("dashboard: %d", dash.Code)
	}
	return b, hidden(e.t, dash.Body.String(), "csrf_token")
}

func (e *testEnv) createRoom(b *browser, csrf, slug string) {
	e.t.Helper()
	rr := b.do(http.MethodPost, "/admin/rooms", url.Values{
		"csrf_token": {csrf}, "slug": {slug}, "host_password": {"host-secret-pw"},
	})
	if rr.Code != http.StatusSeeOther {
		e.t.Fatalf("create room %s: %d %s", slug, rr.Code, rr.Body.String())
	}
}
