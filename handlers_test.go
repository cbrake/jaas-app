package main

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

type testApp struct {
	*App
	server *httptest.Server
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	db := testDB(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)

	app := &App{
		Config: &Config{
			JaaSAppID:     "vpaas-test",
			JaaSKeyID:     "vpaas-test/key1",
			JaaSKey:       key,
			AdminPassword: "admin123",
			SessionSecret: []byte("test-secret-32-bytes-long-xxxxx"),
			ListenAddr:    ":0",
			DBPath:        ":memory:",
		},
		DB:     db,
		DialIn: &DialInClient{BaseURL: "http://localhost:0"},
	}

	mux := http.NewServeMux()
	app.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &testApp{App: app, server: server}
}

func (ta *testApp) adminClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.PostForm(ta.server.URL+"/admin/login", url.Values{
		"password": {"admin123"},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	return client
}

func TestLoginPage(t *testing.T) {
	ta := newTestApp(t)
	resp, err := http.Get(ta.server.URL + "/admin/login")
	if err != nil {
		t.Fatalf("get login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestLoginSuccess(t *testing.T) {
	ta := newTestApp(t)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(ta.server.URL+"/admin/login", url.Values{
		"password": {"admin123"},
	})
	if err != nil {
		t.Fatalf("post login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/admin" {
		t.Errorf("redirect = %q, want /admin", loc)
	}
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == "session" {
			found = true
		}
	}
	if !found {
		t.Error("no session cookie set")
	}
}

func TestLoginWrongPassword(t *testing.T) {
	ta := newTestApp(t)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(ta.server.URL+"/admin/login", url.Values{
		"password": {"wrong"},
	})
	if err != nil {
		t.Fatalf("post login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (re-render with error)", resp.StatusCode)
	}
}

func TestDashboardRequiresAuth(t *testing.T) {
	ta := newTestApp(t)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ta.server.URL + "/admin")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 redirect to login", resp.StatusCode)
	}
}

func TestCreateAndListRoom(t *testing.T) {
	ta := newTestApp(t)
	client := ta.adminClient(t)

	resp, err := client.PostForm(ta.server.URL+"/admin/rooms", url.Values{
		"slug":          {"weekly-sync"},
		"host_password": {"host123"},
	})
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("create: status = %d, want 303", resp.StatusCode)
	}

	room, err := ta.DB.GetRoom("weekly-sync")
	if err != nil {
		t.Fatalf("room not found: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(room.HostHash), []byte("host123")); err != nil {
		t.Error("host password hash doesn't match")
	}
}

func TestWaitingRoom(t *testing.T) {
	ta := newTestApp(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("host123"), bcrypt.DefaultCost)
	ta.DB.CreateRoom("test-room", string(hash))

	resp, err := http.Get(ta.server.URL + "/m/test-room")
	if err != nil {
		t.Fatalf("get meeting: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyBytes), "Join Meeting") {
		t.Error("expected join form page")
	}
}

func TestStartMeeting(t *testing.T) {
	ta := newTestApp(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("host123"), bcrypt.DefaultCost)
	ta.DB.CreateRoom("test-room", string(hash))

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, _ := client.PostForm(ta.server.URL+"/m/test-room/join", url.Values{
		"host_password": {"host123"},
		"display_name":  {"Host User"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/m/test-room?jwt=") {
		t.Errorf("redirect = %q, want /m/test-room?jwt=...", loc)
	}

	room, _ := ta.DB.GetRoom("test-room")
	if !room.Active {
		t.Error("room should be active after start")
	}
}

func TestStartMeetingWrongPassword(t *testing.T) {
	ta := newTestApp(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("host123"), bcrypt.DefaultCost)
	ta.DB.CreateRoom("test-room", string(hash))

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, _ := client.PostForm(ta.server.URL+"/m/test-room/join", url.Values{
		"host_password": {"wrong"},
		"display_name":  {"Host User"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (re-render waiting room with error)", resp.StatusCode)
	}

	room, _ := ta.DB.GetRoom("test-room")
	if room.Active {
		t.Error("room should not be active with wrong password")
	}
}

func TestMeetingPageNotFound(t *testing.T) {
	ta := newTestApp(t)
	resp, err := http.Get(ta.server.URL + "/m/nonexistent")
	if err != nil {
		t.Fatalf("get meeting: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteRoomHandler(t *testing.T) {
	ta := newTestApp(t)
	client := ta.adminClient(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("host123"), bcrypt.DefaultCost)
	ta.DB.CreateRoom("to-delete", string(hash))

	resp, _ := client.PostForm(ta.server.URL+"/admin/rooms/to-delete/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}

	_, err := ta.DB.GetRoom("to-delete")
	if err == nil {
		t.Error("room should be deleted")
	}
}
