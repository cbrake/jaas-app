package main

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

// accessEnv switches a test environment to identity mode: Cloudflare Access
// in front of /admin, domain example.com allowed, admin@ as superadmin.
type accessEnv struct {
	*testEnv
	key *rsa.PrivateKey
}

func newAccessEnv(t *testing.T) *accessEnv {
	t.Helper()
	e := newTestEnv(t)
	key := testKey(t)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(jwks.Close)
	e.app.Config.AdminEmails = []string{"admin@example.com"}
	e.app.CFAccess = &CFAccessVerifier{
		TeamDomain: "example.cloudflareaccess.com", AUD: "aud-123",
		AllowedDomains: []string{"example.com"}, HTTP: jwks.Client(), CertsURL: jwks.URL,
	}
	e.h = e.app.Handler()
	return &accessEnv{testEnv: e, key: key}
}

func (a *accessEnv) assertion(email string) string {
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodRS256, jwtlib.MapClaims{
		"aud": []string{"aud-123"}, "iss": "https://example.cloudflareaccess.com", "email": email,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
	tok.Header["kid"] = "k1"
	s, _ := tok.SignedString(a.key)
	return s
}

// user is a browser that Cloudflare Access has authenticated as email.
func (a *accessEnv) user(email string) (*browser, string) {
	a.t.Helper()
	b := a.browser()
	b.headers = map[string]string{"Cf-Access-Jwt-Assertion": a.assertion(email)}
	rr := b.do(http.MethodGet, "/admin", nil)
	if rr.Code != http.StatusOK {
		a.t.Fatalf("%s dashboard: %d", email, rr.Code)
	}
	return b, hidden(a.t, rr.Body.String(), "csrf_token")
}

func TestIdentityModeNoSharedPassword(t *testing.T) {
	a := newAccessEnv(t)
	alice, _ := a.user("alice@example.com")
	if rr := alice.do(http.MethodGet, "/admin/login", nil); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin" {
		t.Fatalf("login page in identity mode: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	if rr := alice.do(http.MethodGet, "/admin/password", nil); rr.Code != http.StatusSeeOther {
		t.Fatalf("password page in identity mode: %d", rr.Code)
	}
	// Outside the allowed domain: refused even with a valid Access token.
	out := a.browser()
	out.headers = map[string]string{"Cf-Access-Jwt-Assertion": a.assertion("someone@gmail.com")}
	if rr := out.do(http.MethodGet, "/admin", nil); rr.Code != http.StatusForbidden {
		t.Fatalf("foreign domain: %d", rr.Code)
	}
	// Look-alike domain.
	out.headers["Cf-Access-Jwt-Assertion"] = a.assertion("eve@example.com.evil.com")
	if rr := out.do(http.MethodGet, "/admin", nil); rr.Code != http.StatusForbidden {
		t.Fatalf("look-alike domain: %d", rr.Code)
	}
}

func TestIdentityModeRoomsArePrivate(t *testing.T) {
	a := newAccessEnv(t)
	alice, aCSRF := a.user("alice@example.com")
	bob, bCSRF := a.user("bob@example.com")
	admin, _ := a.user("admin@example.com")

	a.createRoom(alice, aCSRF, "alice-client-2026")
	a.createRoom(bob, bCSRF, "bob-client-2026")
	if r, _ := a.app.DB.GetRoom("alice-client-2026"); r.OwnerEmail != "alice@example.com" {
		t.Fatalf("owner: %q", r.OwnerEmail)
	}

	// Minutes for Alice's room.
	a.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "alice-client-2026", testLink))
	drain(a.testEnv)
	ms, _ := a.app.DB.ListMinutes("alice-client-2026")
	ts, _ := a.app.DB.ListTranscriptions("alice-client-2026")
	mid, tid := strconv.FormatInt(ms[0].ID, 10), strconv.FormatInt(ts[0].ID, 10)

	dashA := alice.do(http.MethodGet, "/admin", nil).Body.String()
	dashB := bob.do(http.MethodGet, "/admin", nil).Body.String()
	dashAdmin := admin.do(http.MethodGet, "/admin", nil).Body.String()
	if !strings.Contains(dashA, "alice-client-2026") || strings.Contains(dashA, "bob-client-2026") {
		t.Error("alice sees the wrong rooms")
	}
	if strings.Contains(dashB, "alice-client-2026") {
		t.Error("bob sees alice's room")
	}
	if !strings.Contains(dashAdmin, "alice-client-2026") || !strings.Contains(dashAdmin, "bob-client-2026") || !strings.Contains(dashAdmin, "alice@example.com") {
		t.Error("admin does not see every room with its owner")
	}

	// Bob cannot touch Alice's room or read her minutes/transcripts.
	for _, p := range []string{"delete", "start", "stop", "transcription"} {
		if rr := bob.do(http.MethodPost, "/admin/rooms/alice-client-2026/"+p, url.Values{"csrf_token": {bCSRF}}); rr.Code != http.StatusNotFound {
			t.Errorf("bob %s alice's room: %d", p, rr.Code)
		}
	}
	for _, p := range []string{"/admin/minutes/" + mid, "/admin/minutes/" + mid + "/download", "/admin/transcriptions/" + tid} {
		if rr := bob.do(http.MethodGet, p, nil); rr.Code != http.StatusNotFound {
			t.Errorf("bob GET %s: %d", p, rr.Code)
		}
		if rr := alice.do(http.MethodGet, p, nil); rr.Code != http.StatusOK {
			t.Errorf("alice GET %s: %d", p, rr.Code)
		}
		if rr := admin.do(http.MethodGet, p, nil); rr.Code != http.StatusOK {
			t.Errorf("admin GET %s: %d", p, rr.Code)
		}
	}
	if rr := bob.do(http.MethodPost, "/admin/minutes/"+mid+"/retry", url.Values{"csrf_token": {bCSRF}}); rr.Code != http.StatusNotFound {
		t.Errorf("bob retry alice's minutes: %d", rr.Code)
	}
	if _, err := a.app.DB.GetRoom("alice-client-2026"); err != nil {
		t.Fatal("alice's room was affected by bob")
	}
	// A slug is global: Bob cannot take Alice's URL.
	if rr := bob.do(http.MethodPost, "/admin/rooms", url.Values{"csrf_token": {bCSRF}, "slug": {"alice-client-2026"}, "host_password": {"host-secret-pw"}}); rr.Code != http.StatusConflict {
		t.Errorf("duplicate slug: %d", rr.Code)
	}
	// The owner can delete; the admin can delete anyone's.
	if rr := alice.do(http.MethodPost, "/admin/rooms/alice-client-2026/delete", url.Values{"csrf_token": {aCSRF}}); rr.Code != http.StatusSeeOther {
		t.Errorf("owner delete: %d", rr.Code)
	}
	adminCSRF := hidden(t, admin.do(http.MethodGet, "/admin", nil).Body.String(), "csrf_token")
	if rr := admin.do(http.MethodPost, "/admin/rooms/bob-client-2026/delete", url.Values{"csrf_token": {adminCSRF}}); rr.Code != http.StatusSeeOther {
		t.Errorf("admin delete: %d", rr.Code)
	}
}

func TestIdentityChangeDropsSession(t *testing.T) {
	a := newAccessEnv(t)
	alice, aCSRF := a.user("alice@example.com")
	oldToken := alice.cookies["__Host-jaas_admin"].Value

	// Same browser (Alice's cookie and CSRF), but Access now says Bob.
	alice.headers["Cf-Access-Jwt-Assertion"] = a.assertion("bob@example.com")
	if rr := alice.do(http.MethodPost, "/admin/rooms", url.Values{"csrf_token": {aCSRF}, "slug": {"cross-room"}, "host_password": {"host-secret-pw"}}); rr.Code != http.StatusForbidden {
		t.Fatalf("stale session accepted for a different identity: %d", rr.Code)
	}
	if _, err := a.app.DB.GetRoom("cross-room"); err == nil {
		t.Fatal("room created with someone else's session")
	}
	if _, err := a.app.DB.GetAdminSession(oldToken); err == nil {
		t.Fatal("alice's session not revoked after identity change")
	}
	// Without an Access assertion nothing under /admin answers.
	anon := a.browser()
	anon.cookies = alice.cookies
	if rr := anon.do(http.MethodGet, "/admin", nil); rr.Code != http.StatusForbidden {
		t.Fatalf("cookie without Access assertion: %d", rr.Code)
	}
}

func TestIdentityLogoutEndsAccessSession(t *testing.T) {
	a := newAccessEnv(t)
	alice, csrf := a.user("alice@example.com")
	token := alice.cookies["__Host-jaas_admin"].Value
	rr := alice.do(http.MethodPost, "/admin/logout", url.Values{"csrf_token": {csrf}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/cdn-cgi/access/logout" {
		t.Fatalf("logout: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	if _, err := a.app.DB.GetAdminSession(token); err == nil {
		t.Fatal("session not revoked")
	}
}

func TestMigrationAddsOwnerToExistingDatabase(t *testing.T) {
	e := newTestEnv(t)
	var v int
	e.app.DB.conn.QueryRow("PRAGMA user_version").Scan(&v)
	if v != len(migrations) {
		t.Fatalf("schema version %d, want %d", v, len(migrations))
	}
	if err := e.app.DB.CreateRoom("legacy-room", "x", ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.app.DB.GetRoom("legacy-room"); r.OwnerEmail != "" {
		t.Fatal("legacy room should have no owner")
	}
}
