package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Official example from https://developer.8x8.com/jaas/docs/webhooks-signatures/
const (
	officialSecret = "whsec_9635df66714a4cf088ee9d0979dd3bf6"
	officialHeader = "t=1632490060,v1=xlzqEojlh4qb21sQpXYsWgyK8x9HVpz+RQldsv18rV0="
	officialBody   = `{"eventType":"PARTICIPANT_JOINED","sessionId":"9a441d60-ceaf-4eba-b0a8-a7d940a76e1b","timestamp":1632490058278,"fqn":"vpaas-magic-cookie-96f0941768964ab380ed0fbada7a502f/sampleappromanticshiftsstripas","idempotencyKey":"9e9e7420-562d-4659-8e22-44b9b22aaa49","customerId":"96f0941768964ab380ed0fbada7a502f","appId":"vpaas-magic-cookie-96f0941768964ab380ed0fbada7a502f","data":{"avatar":"","name":"Test User","id":"auth0|5f903d7a77f3b4006eb8e67d","participantJid":"fc1ea14a-9bca-4218-a563-8c627e803d56@8x8.vc","moderator":true,"email":"test.user@company.com"}}`
)

func TestSignatureOfficialVector(t *testing.T) {
	at := time.Unix(1632490060, 0).Add(10 * time.Second)
	if err := verifyJaaSSignature(officialHeader, []byte(officialBody), []byte(officialSecret), 5*time.Minute, at); err != nil {
		t.Fatalf("official vector rejected: %v", err)
	}
}

func TestSignatureRejections(t *testing.T) {
	at := time.Unix(1632490060, 0)
	body := []byte(officialBody)
	secret := []byte(officialSecret)
	sig := "xlzqEojlh4qb21sQpXYsWgyK8x9HVpz+RQldsv18rV0="
	cases := map[string]struct {
		header string
		body   []byte
		secret []byte
		now    time.Time
	}{
		"empty header":       {"", body, secret, at},
		"wrong secret":       {officialHeader, body, []byte("whsec_other"), at},
		"tampered body":      {officialHeader, []byte(strings.Replace(officialBody, "Test User", "Mallory", 1)), secret, at},
		"replayed (stale)":   {officialHeader, body, secret, at.Add(6 * time.Minute)},
		"from the future":    {officialHeader, body, secret, at.Add(-6 * time.Minute)},
		"downgrade to v0":    {"t=1632490060,v0=" + sig, body, secret, at},
		"missing timestamp":  {"v1=" + sig, body, secret, at},
		"timestamp mismatch": {"t=1632490061,v1=" + sig, body, secret, at},
		"no secret config":   {officialHeader, body, nil, at},
	}
	for name, c := range cases {
		if err := verifyJaaSSignature(c.header, c.body, c.secret, 5*time.Minute, c.now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Several v1 signatures (secret rotation): one valid is enough.
	if err := verifyJaaSSignature("t=1632490060,v1=AAAA,v1="+sig, body, secret, 5*time.Minute, at); err != nil {
		t.Errorf("multiple v1: %v", err)
	}
}

func TestValidateDownloadURL(t *testing.T) {
	allowed := []string{"objectstorage.*.oraclecloud.com"}
	ok := []string{
		"https://objectstorage.us-phoenix-1.oraclecloud.com/p/abc/n/x/b/y/o/file.json",
		"https://objectstorage.eu-frankfurt-1.oraclecloud.com:443/p/abc",
	}
	bad := []string{
		"http://objectstorage.us-phoenix-1.oraclecloud.com/p/abc",
		"https://user:pw@objectstorage.us-phoenix-1.oraclecloud.com/p/abc",
		"https://objectstorage.us-phoenix-1.oraclecloud.com:8443/p/abc",
		"https://169.254.169.254/latest/meta-data/",
		"https://127.0.0.1/",
		"https://[::1]/",
		"https://objectstorage.us-phoenix-1.oraclecloud.com.evil.example/p",
		"https://evil-objectstorage.us-phoenix-1.oraclecloud.com/p",
		"https://objectstorage.a.b.oraclecloud.com/p",
		"https://oraclecloud.com/p",
		"javascript:alert(1)",
		"file:///etc/passwd",
		"",
	}
	for _, u := range ok {
		if _, err := validateDownloadURL(u, allowed); err != nil {
			t.Errorf("rejected allowed URL %s", u)
		}
	}
	for _, u := range bad {
		if _, err := validateDownloadURL(u, allowed); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.17.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1"} {
		if isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "140.91.1.1", "2606:4700::1111"} {
		if !isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as private", s)
		}
	}
}

// Even if an allowed hostname resolved to an internal address, the dialer
// refuses to connect.
func TestDownloadClientRefusesInternalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("internal")) }))
	defer srv.Close()
	c := newDownloadClient([]string{"objectstorage.*.oraclecloud.com"})
	resp, err := c.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("connected to a loopback address")
	}
}

func TestWebhookRequiresSignature(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	body := eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink)

	if rr := e.postWebhook(body, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", rr.Code)
	}
	if rr := e.postWebhook(body, signBody("whsec_wrong", body, e.now)); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad secret: %d", rr.Code)
	}
	if rr := e.postWebhook(body, signBody(testSecret, body, e.now.Add(-10*time.Minute))); rr.Code != http.StatusUnauthorized {
		t.Fatalf("replay: %d", rr.Code)
	}
	if n, _ := e.app.DB.CountMinutesForSession("test-room", "s1"); n != 0 {
		t.Fatalf("unsigned webhook created %d jobs", n)
	}
}

func TestWebhookIdempotency(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")

	first := eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink)
	for i := 0; i < 3; i++ { // JaaS retrying the same delivery
		if rr := e.signedWebhook(first); rr.Code != http.StatusOK {
			t.Fatalf("delivery %d: %d", i, rr.Code)
		}
	}
	// A different delivery (new idempotencyKey) for the same session.
	if rr := e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k2", "s1", "test-room", testLink)); rr.Code != http.StatusOK {
		t.Fatalf("second key: %d", rr.Code)
	}
	if n, _ := e.app.DB.CountMinutesForSession("test-room", "s1"); n != 1 {
		t.Fatalf("want exactly 1 minutes job, got %d", n)
	}
	if !strings.Contains(e.logs.String(), "webhook: duplicate") {
		t.Error("duplicate not logged")
	}
}

func TestWebhookIgnoresForeignAndUnknown(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")

	foreign := strings.ReplaceAll(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink), testAppID, "vpaas-magic-cookie-other")
	if rr := e.signedWebhook(foreign); rr.Code != http.StatusOK {
		t.Fatalf("foreign: %d", rr.Code)
	}
	if rr := e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k2", "s2", "no-existe", testLink)); rr.Code != http.StatusOK {
		t.Fatalf("unknown room: %d", rr.Code)
	}
	if rr := e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k3", "s3", "../etc", testLink)); rr.Code != http.StatusOK {
		t.Fatalf("bad slug: %d", rr.Code)
	}
	for _, s := range []string{"s1", "s2", "s3"} {
		if n, _ := e.app.DB.CountMinutesForSession("test-room", s); n != 0 {
			t.Fatalf("job created for %s", s)
		}
	}
}

func TestWebhookRejectsDisallowedLinks(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	for i, link := range []string{
		"http://169.254.169.254/latest/meta-data/iam",
		"https://internal.example/secret?token=abc",
		"javascript:alert(document.cookie)",
	} {
		key := "bad-" + string(rune('a'+i))
		rr := e.signedWebhook(eventJSON("RECORDING_UPLOADED", key, "s1", "test-room", link))
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d", link, rr.Code)
		}
	}
	recs, _ := e.app.DB.ListRecordings("test-room")
	if len(recs) != 0 {
		t.Fatalf("stored %d recordings with disallowed links", len(recs))
	}
	if strings.Contains(e.logs.String(), "token=abc") || strings.Contains(e.logs.String(), "/latest/meta-data") {
		t.Error("rejected link path/query leaked to logs")
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	e := newTestEnv(t)
	big := `{"pad":"` + strings.Repeat("a", 2<<20) + `"}`
	if rr := e.signedWebhook(big); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", rr.Code)
	}
}

func TestWebhookStoresRecording(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	if rr := e.signedWebhook(eventJSON("RECORDING_UPLOADED", "r1", "s1", "test-room", testLink)); rr.Code != http.StatusOK {
		t.Fatalf("recording: %d", rr.Code)
	}
	recs, _ := e.app.DB.ListRecordings("test-room")
	if len(recs) != 1 || recs[0].DownloadURL != testLink {
		t.Fatalf("recordings: %+v", recs)
	}
	if strings.Contains(e.logs.String(), "SECRET-PREAUTH-TOKEN") {
		t.Error("pre-authenticated link leaked to logs")
	}
}

func TestWebhookTolerantPayloads(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("test-room", "x", "")
	fqn := testAppID + "/test-room"
	cases := map[string]string{
		// numbers as strings / with decimals, flags as strings
		"string timestamp": `{"idempotencyKey":"a1","appId":"` + testAppID + `","eventType":"TRANSCRIPTION_UPLOADED","sessionId":"s1","timestamp":"1760464800000","fqn":"` + fqn + `","data":{"preAuthenticatedLink":"` + testLink + `"}}`,
		"float duration":   `{"idempotencyKey":"a2","appId":"` + testAppID + `","eventType":"RECORDING_UPLOADED","sessionId":"s1","timestamp":1.7604648e12,"fqn":"` + fqn + `","data":{"preAuthenticatedLink":"` + testLink + `","durationSec":15.5,"linkExpiration":"1792087200000","participants":[{"name":"A"}]}}`,
		"string moderator": `{"idempotencyKey":"a3","appId":"` + testAppID + `","eventType":"PARTICIPANT_JOINED","sessionId":"s1","timestamp":1760464800000,"fqn":"` + fqn + `","data":{"name":"Ana","email":"ana@client.example","moderator":"true","id":123}}`,
		// events we do not act on: acknowledged even without the usual fields
		"unknown, no key":   `{"eventType":"TRANSCRIPTION_CHUNK_RECEIVED","fqn":"` + fqn + `","data":{"final":"hello","participant":{"name":"A"}}}`,
		"unknown data type": `{"idempotencyKey":"a5","eventType":"USAGE","fqn":"` + fqn + `","data":[1,2,3]}`,
	}
	for name, body := range cases {
		if rr := e.signedWebhook(body); rr.Code != http.StatusOK {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
	if n, _ := e.app.DB.CountMinutesForSession("test-room", "s1"); n != 1 {
		t.Errorf("transcription with string timestamp not queued: %d", n)
	}
	if recs, _ := e.app.DB.ListRecordings("test-room"); len(recs) != 1 || recs[0].DurationSec != 15 {
		t.Errorf("recording with float duration: %+v", recs)
	}
	var mod bool
	e.app.DB.conn.QueryRow("SELECT moderator FROM session_participants WHERE source = 'jaas'").Scan(&mod)
	if !mod {
		t.Error("string moderator flag not read")
	}
	// Still rejected: not JSON, no eventType, actionable without idempotencyKey.
	for name, body := range map[string]string{
		"not json":       `hola`,
		"no eventType":   `{"idempotencyKey":"b1"}`,
		"actionable/key": `{"eventType":"TRANSCRIPTION_UPLOADED","fqn":"` + fqn + `"}`,
	} {
		if rr := e.signedWebhook(body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
	if !strings.Contains(e.logs.String(), "payload: missing idempotencyKey") || strings.Contains(e.logs.String(), "hello") {
		t.Error("rejection reasons not logged safely")
	}
}

// JaaS retries keep the signature timestamp of the first attempt.
func TestWebhookRetryWithOriginalTimestamp(t *testing.T) {
	e := newTestEnv(t)
	e.app.Config.WebhookTolerance = 24 * time.Hour
	e.app.DB.CreateRoom("test-room", "x", "")
	body := eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "test-room", testLink)
	sig := signBody(testSecret, body, e.now)
	e.advance(7 * time.Minute)
	if rr := e.postWebhook(body, sig); rr.Code != http.StatusOK {
		t.Fatalf("retry 7 min later: %d", rr.Code)
	}
	if rr := e.postWebhook(body, sig); rr.Code != http.StatusOK || !strings.Contains(e.logs.String(), "webhook: duplicate") {
		t.Fatal("replay not de-duplicated")
	}
	e.advance(25 * time.Hour)
	if rr := e.postWebhook(body, sig); rr.Code != http.StatusUnauthorized || !strings.Contains(e.logs.String(), "signature: stale timestamp") {
		t.Fatalf("day-old signature: %d", rr.Code)
	}
}
