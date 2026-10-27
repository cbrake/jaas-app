package main

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

func TestSecurityHeaders(t *testing.T) {
	e := newTestEnv(t)
	rr := e.browser().do(http.MethodGet, "/", nil)
	h := rr.Header()
	for k, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
		"Cache-Control":          "no-store",
	} {
		if h.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, h.Get(k), want)
		}
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("csp: %s", csp)
	}
}

func TestAdminLoginCookiesAndTemporaryPassword(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.SetAdminPassword(testAdminPW, true)
	b := e.browser()
	page := b.do(http.MethodGet, "/admin/login", nil)
	formToken := hidden(t, page.Body.String(), "form_token")

	// Cross-site POST (wrong Origin) and missing form token are refused.
	evil := e.browser()
	evil.cookies = b.cookies
	evil.origin = "https://evil.example"
	if rr := evil.do(http.MethodPost, "/admin/login", url.Values{"form_token": {formToken}, "password": {testAdminPW}}); rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d", rr.Code)
	}
	if rr := b.do(http.MethodPost, "/admin/login", url.Values{"password": {testAdminPW}}); rr.Code != http.StatusForbidden {
		t.Fatalf("login without form token: %d", rr.Code)
	}

	rr := b.do(http.MethodPost, "/admin/login", url.Values{"form_token": {formToken}, "password": {testAdminPW}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/password" {
		t.Fatalf("login: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "__Host-tgm_admin" {
			session = c
		}
	}
	if session == nil || !session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" || session.Domain != "" {
		t.Fatalf("admin cookie flags: %+v", session)
	}
	if len(session.Value) < 40 || strings.Contains(session.Value, "authenticated") {
		t.Fatalf("session token is not random: %q", session.Value)
	}

	// Temporary password: everything else redirects to the change page.
	if rr := b.do(http.MethodGet, "/admin", nil); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/password" {
		t.Fatalf("must-change not enforced: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	page = b.do(http.MethodGet, "/admin/password", nil)
	csrf := hidden(t, page.Body.String(), "csrf_token")
	if rr := b.do(http.MethodPost, "/admin/password", url.Values{"current_password": {testAdminPW}, "new_password": {"x"}}); rr.Code != http.StatusForbidden {
		t.Fatalf("password change without csrf: %d", rr.Code)
	}
	if rr := b.do(http.MethodPost, "/admin/password", url.Values{"csrf_token": {csrf}, "current_password": {testAdminPW},
		"new_password": {"corta"}, "confirm_password": {"corta"}}); rr.Code != http.StatusBadRequest {
		t.Fatalf("short password accepted: %d", rr.Code)
	}
	if rr := b.do(http.MethodPost, "/admin/password", url.Values{"csrf_token": {csrf}, "current_password": {testAdminPW},
		"new_password": {testAdminPW}, "confirm_password": {testAdminPW}}); rr.Code != http.StatusBadRequest {
		t.Fatalf("reused password accepted: %d", rr.Code)
	}
	oldCookie := b.cookies["__Host-tgm_admin"].Value
	if rr := b.do(http.MethodPost, "/admin/password", url.Values{"csrf_token": {csrf}, "current_password": {testAdminPW},
		"new_password": {"Nueva-Contrasena-Segura-2026"}, "confirm_password": {"Nueva-Contrasena-Segura-2026"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("password change: %d", rr.Code)
	}
	if rr := b.do(http.MethodGet, "/admin", nil); rr.Code != http.StatusOK {
		t.Fatalf("dashboard after change: %d", rr.Code)
	}
	// The pre-change session was revoked server side.
	stale := e.browser()
	stale.cookies["__Host-tgm_admin"] = &http.Cookie{Name: "__Host-tgm_admin", Value: oldCookie}
	if rr := stale.do(http.MethodGet, "/admin", nil); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/login" {
		t.Fatalf("old session still valid: %d", rr.Code)
	}
	// The temporary password no longer works.
	if cred, _ := e.app.DB.GetAdminCredential(); cred.MustChange {
		t.Fatal("must_change still set")
	}
}

func TestAdminSessionExpiryAndLogout(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()

	e.advance(adminIdleTimeout + time.Minute)
	if rr := b.do(http.MethodGet, "/admin", nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("idle session still valid: %d", rr.Code)
	}

	b, csrf = e.loginAdmin()
	for i := 0; i < 13; i++ { // stay active, but exceed the absolute lifetime
		e.advance(time.Hour)
		b.do(http.MethodGet, "/admin", nil)
	}
	if rr := b.do(http.MethodGet, "/admin", nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("session outlived absolute timeout: %d", rr.Code)
	}

	b, csrf = e.loginAdmin()
	token := b.cookies["__Host-tgm_admin"].Value
	if rr := b.do(http.MethodPost, "/admin/logout", url.Values{"csrf_token": {csrf}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", rr.Code)
	}
	if _, err := e.app.DB.GetAdminSession(token); err == nil {
		t.Fatal("logout did not revoke the session server side")
	}
}

func TestAdminLoginRateLimit(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.SetAdminPassword(testAdminPW, true)
	b := e.browser()
	tok := hidden(t, b.do(http.MethodGet, "/admin/login", nil).Body.String(), "form_token")
	for i := 0; i < 5; i++ {
		if rr := b.do(http.MethodPost, "/admin/login", url.Values{"form_token": {tok}, "password": {"wrong"}}); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rr.Code)
		}
	}
	if rr := b.do(http.MethodPost, "/admin/login", url.Values{"form_token": {tok}, "password": {testAdminPW}}); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt (correct password) not blocked: %d", rr.Code)
	}
	// Another IP is not affected.
	other := e.browser()
	other.ip = "198.51.100.7"
	tok2 := hidden(t, other.do(http.MethodGet, "/admin/login", nil).Body.String(), "form_token")
	if rr := other.do(http.MethodPost, "/admin/login", url.Values{"form_token": {tok2}, "password": {testAdminPW}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("other IP: %d", rr.Code)
	}
	e.advance(16 * time.Minute)
	if rr := b.do(http.MethodPost, "/admin/login", url.Values{"form_token": {tok}, "password": {testAdminPW}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("not unblocked after window: %d", rr.Code)
	}
}

func TestClientIPFromTrustedProxyOnly(t *testing.T) {
	e := newTestEnv(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	r.Header.Set("X-Real-IP", "1.2.3.4")
	if ip := e.app.clientIP(r); ip != "203.0.113.5" {
		t.Errorf("spoofed X-Real-IP trusted from untrusted peer: %s", ip)
	}
	r.RemoteAddr = "127.0.0.1:1234"
	if ip := e.app.clientIP(r); ip != "1.2.3.4" {
		t.Errorf("X-Real-IP from proxy ignored: %s", ip)
	}
}

func TestAdminPostsRequireCSRFAndOrigin(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	form := url.Values{"slug": {"sala-csrf"}, "host_password": {"anfitrion-secreto"}}

	if rr := b.do(http.MethodPost, "/admin/rooms", form); rr.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", rr.Code)
	}
	form.Set("csrf_token", "not-the-token")
	if rr := b.do(http.MethodPost, "/admin/rooms", form); rr.Code != http.StatusForbidden {
		t.Fatalf("wrong csrf: %d", rr.Code)
	}
	form.Set("csrf_token", csrf)
	b.origin = "https://evil.example"
	if rr := b.do(http.MethodPost, "/admin/rooms", form); rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", rr.Code)
	}
	b.origin = ""
	if rr := b.do(http.MethodPost, "/admin/rooms", form); rr.Code != http.StatusForbidden {
		t.Fatalf("no Origin/Referer: %d", rr.Code)
	}
	b.origin = testOrigin
	if rr := b.do(http.MethodPost, "/admin/rooms", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("valid request: %d", rr.Code)
	}
	if _, err := e.app.DB.GetRoom("sala-csrf"); err != nil {
		t.Fatal("room not created")
	}
}

func TestSlugValidation(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	for _, slug := range []string{"ab", "-abc", "Sala", "sala_1", "sala con espacio", "../etc", strings.Repeat("a", 81), "sala/otra", "<script>"} {
		rr := b.do(http.MethodPost, "/admin/rooms", url.Values{"csrf_token": {csrf}, "slug": {slug}, "host_password": {"anfitrion-secreto"}})
		if rr.Code != http.StatusBadRequest {
			t.Errorf("slug %q: %d", slug, rr.Code)
		}
	}
	e.createRoom(b, csrf, "transgenia-poliarte-odooia-2026")
	if rr := e.browser().do(http.MethodGet, "/m/transgenia-poliarte-odooia-2026", nil); rr.Code != http.StatusOK {
		t.Fatalf("valid room page: %d", rr.Code)
	}
	if rr := e.browser().do(http.MethodGet, "/m/NOPE", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("invalid slug page: %d", rr.Code)
	}
}

func TestPasswordFieldsAreMasked(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "sala-prueba")
	pages := map[string]string{
		"dashboard": b.do(http.MethodGet, "/admin", nil).Body.String(),
		"join":      e.browser().do(http.MethodGet, "/m/sala-prueba", nil).Body.String(),
	}
	guest := e.browser()
	tok := hidden(t, guest.do(http.MethodGet, "/m/sala-prueba", nil).Body.String(), "form_token")
	pages["waiting"] = guest.do(http.MethodPost, "/m/sala-prueba/join", url.Values{"form_token": {tok}, "display_name": {"Invitada"}, "email": {"invitada@cliente.example"}, "consent": {"on"}}).Body.String()
	input := regexp.MustCompile(`<input[^>]*name="host_password"[^>]*>`)
	for name, html := range pages {
		found := input.FindAllString(html, -1)
		if len(found) == 0 {
			t.Errorf("%s: no host_password input", name)
		}
		for _, in := range found {
			if !strings.Contains(in, `type="password"`) {
				t.Errorf("%s: host password not masked: %s", name, in)
			}
		}
	}
}

func jwtClaims(t *testing.T, e *testEnv, html string) jwtlib.MapClaims {
	t.Helper()
	m := regexp.MustCompile(`data-jwt="([^"]+)"`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no JWT on meeting page")
	}
	claims := jwtlib.MapClaims{}
	if _, err := jwtlib.ParseWithClaims(m[1], claims, func(*jwtlib.Token) (any, error) { return &e.app.Config.JaaSKey.PublicKey, nil }); err != nil {
		t.Fatalf("jwt: %v", err)
	}
	return claims
}

func isModerator(c jwtlib.MapClaims) bool {
	return c["context"].(map[string]any)["user"].(map[string]any)["moderator"].(bool)
}

func TestMeetingFlowKeepsJWTOutOfURLs(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "sala-prueba")

	guest := e.browser()
	tok := hidden(t, guest.do(http.MethodGet, "/m/sala-prueba", nil).Body.String(), "form_token")
	rr := guest.do(http.MethodPost, "/m/sala-prueba/join", url.Values{"form_token": {tok}, "display_name": {"Invitada"}, "email": {"invitada@cliente.example"}, "consent": {"on"}})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Esperando") {
		t.Fatalf("guest before host should wait: %d", rr.Code)
	}

	host := e.browser()
	host.ip = "198.51.100.20"
	tok = hidden(t, host.do(http.MethodGet, "/m/sala-prueba", nil).Body.String(), "form_token")
	rr = host.do(http.MethodPost, "/m/sala-prueba/join", url.Values{"form_token": {tok}, "display_name": {"Saurat"}, "email": {"saurat@cliente.example"}, "consent": {"on"}, "host_password": {"anfitrion-secreto"}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/m/sala-prueba" {
		t.Fatalf("host join: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	page := host.do(http.MethodGet, "/m/sala-prueba", nil)
	if !isModerator(jwtClaims(t, e, page.Body.String())) {
		t.Fatal("host is not moderator")
	}
	if page.Header().Get("Cache-Control") != "no-store" || !strings.Contains(page.Header().Get("Content-Security-Policy"), "frame-src https://8x8.vc") {
		t.Fatalf("meeting headers: %v", page.Header())
	}
	if !strings.Contains(page.Header().Get("Permissions-Policy"), `camera=(self "https://8x8.vc")`) {
		t.Fatal("meeting page does not delegate camera to the JaaS iframe")
	}

	tok = hidden(t, guest.do(http.MethodGet, "/m/sala-prueba", nil).Body.String(), "form_token")
	rr = guest.do(http.MethodPost, "/m/sala-prueba/join", url.Values{"form_token": {tok}, "display_name": {"Invitada"}, "email": {"invitada@cliente.example"}, "consent": {"on"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("guest join after host: %d", rr.Code)
	}
	if isModerator(jwtClaims(t, e, guest.do(http.MethodGet, "/m/sala-prueba", nil).Body.String())) {
		t.Fatal("guest received a moderator JWT")
	}
	for _, line := range strings.Split(e.logs.String(), "\n") {
		if strings.Contains(line, "eyJ") || strings.Contains(line, "jwt=") {
			t.Fatalf("JWT in logs: %s", line)
		}
	}
}

func TestHostPasswordRateLimit(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "sala-prueba")
	g := e.browser()
	tok := hidden(t, g.do(http.MethodGet, "/m/sala-prueba", nil).Body.String(), "form_token")
	for i := 0; i < 5; i++ {
		if rr := g.do(http.MethodPost, "/m/sala-prueba/join", url.Values{"form_token": {tok}, "display_name": {"X"}, "email": {"x@cliente.example"}, "consent": {"on"}, "host_password": {"mala"}}); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, rr.Code)
		}
	}
	if rr := g.do(http.MethodPost, "/m/sala-prueba/join", url.Values{"form_token": {tok}, "display_name": {"X"}, "email": {"x@cliente.example"}, "consent": {"on"}, "host_password": {"anfitrion-secreto"}}); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("not rate limited: %d", rr.Code)
	}
}

func TestEndMeetingRequiresModeratorSession(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "sala-prueba")
	e.app.DB.SetRoomActive("sala-prueba", true)
	active := func() bool { r, _ := e.app.DB.GetRoom("sala-prueba"); return r.Active }

	anon := e.browser()
	if rr := anon.do(http.MethodPost, "/m/sala-prueba/end", url.Values{}); rr.Code != http.StatusForbidden || !active() {
		t.Fatalf("anonymous end: %d", rr.Code)
	}
	guest := e.browser()
	tok := hidden(t, guest.do(http.MethodGet, "/m/sala-prueba", nil).Body.String(), "form_token")
	guest.do(http.MethodPost, "/m/sala-prueba/join", url.Values{"form_token": {tok}, "display_name": {"Invitada"}, "email": {"invitada@cliente.example"}, "consent": {"on"}})
	gtok := regexp.MustCompile(`data-csrf="([^"]+)"`).FindStringSubmatch(guest.do(http.MethodGet, "/m/sala-prueba", nil).Body.String())
	if gtok == nil {
		t.Fatal("guest page has no CSRF token for audio uploads")
	}
	if rr := guest.do(http.MethodPost, "/m/sala-prueba/end", url.Values{"csrf_token": {gtok[1]}}); rr.Code != http.StatusForbidden || !active() {
		t.Fatalf("guest end: %d", rr.Code)
	}

	host := e.browser()
	rr := b.do(http.MethodPost, "/admin/rooms/sala-prueba/start", url.Values{"csrf_token": {csrf}, "display_name": {"Anfitrión"}, "email": {"anfitrion@cliente.example"}, "consent": {"on"}})
	if rr.Code != http.StatusSeeOther || strings.Contains(rr.Header().Get("Location"), "jwt") {
		t.Fatalf("admin start: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	host.cookies = b.cookies
	page := host.do(http.MethodGet, "/m/sala-prueba", nil).Body.String()
	hcsrf := regexp.MustCompile(`data-csrf="([^"]+)"`).FindStringSubmatch(page)[1]
	if rr := host.do(http.MethodPost, "/m/sala-prueba/end", url.Values{}); rr.Code != http.StatusForbidden || !active() {
		t.Fatalf("moderator end without csrf: %d", rr.Code)
	}
	if rr := host.do(http.MethodPost, "/m/sala-prueba/end", url.Values{"csrf_token": {hcsrf}}); rr.Code != http.StatusNoContent || active() {
		t.Fatalf("moderator end: %d", rr.Code)
	}
}

func TestDeleteRoomRemovesRelatedData(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "sala-borrar")
	e.createRoom(b, csrf, "sala-conservar")
	for _, slug := range []string{"sala-borrar", "sala-conservar"} {
		e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "t-"+slug, "s1", slug, testLink))
		e.signedWebhook(eventJSON("RECORDING_UPLOADED", "r-"+slug, "s1", slug, testLink))
	}
	for e.app.processOne(context.Background()) {
	}
	e.app.DB.CreateParticipantSession("sala-borrar", "X", "x@cliente.example", false, time.Hour)

	if rr := b.do(http.MethodPost, "/admin/rooms/sala-borrar/delete", url.Values{"csrf_token": {csrf}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rr.Code)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := e.app.DB.conn.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, table := range []string{"recordings", "transcriptions", "minutes", "participant_sessions"} {
		if n := count("SELECT COUNT(*) FROM "+table+" WHERE room_slug = ?", "sala-borrar"); n != 0 {
			t.Errorf("%s: %d rows left", table, n)
		}
	}
	if n := count("SELECT COUNT(*) FROM webhook_events WHERE room_slug = ? OR session_id IS NOT NULL AND idempotency_key LIKE '%-sala-borrar'", "sala-borrar"); n != 0 {
		t.Errorf("webhook events still reference the room: %d", n)
	}
	if n := count("SELECT COUNT(*) FROM webhook_events WHERE idempotency_key = 't-sala-borrar'"); n != 1 {
		t.Errorf("idempotency key should be kept anonymised, got %d", n)
	}
	if n := count("SELECT COUNT(*) FROM minutes WHERE room_slug = 'sala-conservar'"); n != 1 {
		t.Errorf("other room affected: %d", n)
	}
}

func TestCloudflareAccessGate(t *testing.T) {
	e := newTestEnv(t)
	key := testKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer jwks.Close()
	e.app.CFAccess = &CFAccessVerifier{TeamDomain: "transgenia.cloudflareaccess.com", AUD: "aud-123",
		AllowedEmails: []string{"admin@transgenia.org"}, HTTP: jwks.Client(), CertsURL: jwks.URL}
	e.h = e.app.Handler()

	sign := func(k *rsa.PrivateKey, aud, email string) string {
		tok := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
			"aud": []string{aud}, "iss": "https://transgenia.cloudflareaccess.com", "email": email,
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		})
		tok.Header["kid"] = "k1"
		s, _ := tok.SignedString(k)
		return s
	}
	get := func(assertion string) int {
		req := httptest.NewRequest(http.MethodGet, testOrigin+"/admin", nil)
		if assertion != "" {
			req.Header.Set("Cf-Access-Jwt-Assertion", assertion)
		}
		rr := httptest.NewRecorder()
		e.h.ServeHTTP(rr, req)
		return rr.Code
	}
	cases := map[string]struct {
		assertion string
		want      int
	}{
		"missing (origin hit directly)": {"", http.StatusForbidden},
		"valid":                         {sign(key, "aud-123", "admin@transgenia.org"), http.StatusOK},
		"wrong audience":                {sign(key, "aud-other", "admin@transgenia.org"), http.StatusForbidden},
		"email not allowed":             {sign(key, "aud-123", "someone@example.com"), http.StatusForbidden},
		"forged key":                    {sign(testKey(t), "aud-123", "admin@transgenia.org"), http.StatusForbidden},
	}
	for name, c := range cases {
		if got := get(c.assertion); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	// Public routes are not behind Access.
	if rr := e.browser().do(http.MethodGet, "/", nil); rr.Code != http.StatusOK {
		t.Errorf("home behind access: %d", rr.Code)
	}
}

func TestServerHasTimeouts(t *testing.T) {
	// Guard against regressions to http.ListenAndServe without limits.
	src := mustRead(t, "main.go")
	for _, want := range []string{"ReadHeaderTimeout", "ReadTimeout", "WriteTimeout", "IdleTimeout", "MaxHeaderBytes"} {
		if !strings.Contains(src, want) {
			t.Errorf("http.Server missing %s", want)
		}
	}
}

func TestStaticAssetsAreVersioned(t *testing.T) {
	e := newTestEnv(t)
	page := e.browser().do(http.MethodGet, "/", nil).Body.String()
	m := regexp.MustCompile(`/static/app\.css\?v=([0-9a-f]{12})`).FindStringSubmatch(page)
	if m == nil || m[1] != assetVersions["app.css"] {
		t.Fatalf("app.css not versioned in page")
	}
	rr := e.browser().do(http.MethodGet, "/static/app.css?v="+m[1], nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("current version: %d %s", rr.Code, rr.Header().Get("Cache-Control"))
	}
	for _, p := range []string{"/static/app.css", "/static/app.css?v=000000000000"} {
		if cc := e.browser().do(http.MethodGet, p, nil).Header().Get("Cache-Control"); cc != "public, max-age=60" {
			t.Errorf("%s: %s", p, cc)
		}
	}
	for _, name := range []string{"meeting.js", "join.js", "dashboard.js", "logo.svg"} {
		if assetVersions[name] == "" {
			t.Errorf("no version for %s", name)
		}
	}
}

func TestMeetingsLastUpTo24Hours(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "sala-larga")
	host := e.browser()
	tok := hidden(t, host.do(http.MethodGet, "/m/sala-larga", nil).Body.String(), "form_token")
	form := url.Values{"form_token": {tok}, "display_name": {"Ana"}, "email": {"ana@cliente.example"}, "consent": {"on"}, "host_password": {"anfitrion-secreto"}}
	if rr := host.do(http.MethodPost, "/m/sala-larga/join", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("host join: %d", rr.Code)
	}
	claims := jwtClaims(t, e, host.do(http.MethodGet, "/m/sala-larga", nil).Body.String())
	exp, _ := claims.GetExpirationTime()
	if d := exp.Sub(time.Now()); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("JWT lifetime %v, want ~24 h", d)
	}
	e.advance(23 * time.Hour)
	e.app.DB.DeactivateExpiredRooms(e.app.maxMeeting())
	if r, _ := e.app.DB.GetRoom("sala-larga"); !r.Active {
		t.Fatal("room closed before 24 h")
	}
	if rr := host.do(http.MethodGet, "/m/sala-larga", nil); !strings.Contains(rr.Body.String(), "data-jwt=") {
		t.Fatal("participant session expired before 24 h")
	}
	e.advance(2 * time.Hour)
	e.app.DB.DeactivateExpiredRooms(e.app.maxMeeting())
	if r, _ := e.app.DB.GetRoom("sala-larga"); r.Active {
		t.Fatal("room still active after 25 h")
	}
}

func TestTransgeniaBrandEverywhere(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "sala-marca")
	host := e.browser()
	tok := hidden(t, host.do(http.MethodGet, "/m/sala-marca", nil).Body.String(), "form_token")
	host.do(http.MethodPost, "/m/sala-marca/join", url.Values{"form_token": {tok}, "display_name": {"Ana"}, "email": {"ana@cliente.example"}, "consent": {"on"}, "host_password": {"anfitrion-secreto"}})
	meeting := host.do(http.MethodGet, "/m/sala-marca", nil).Body.String()
	if !strings.Contains(meeting, `class="brand-overlay"`) || !strings.Contains(meeting, "transgenia-logo-alt.png?v=") || !strings.Contains(meeting, "IA aplicada") {
		t.Error("meeting page lacks the Transgenia logo overlay")
	}
	for name, page := range map[string]string{
		"home":  e.browser().do(http.MethodGet, "/", nil).Body.String(),
		"join":  e.browser().do(http.MethodGet, "/m/sala-marca", nil).Body.String(),
		"panel": b.do(http.MethodGet, "/admin", nil).Body.String(),
	} {
		if !strings.Contains(page, "Transgenia: IA aplicada") || !strings.Contains(page, "transgenia-logo.png?v=") {
			t.Errorf("%s lacks logo or tagline", name)
		}
	}
	if rr := e.browser().do(http.MethodGet, "/static/transgenia-logo-alt.png", nil); rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/png" {
		t.Errorf("logo asset: %d %s", rr.Code, rr.Header().Get("Content-Type"))
	}
}
