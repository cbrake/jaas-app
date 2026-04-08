package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSignAndValidateSession(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-xxxxx")
	cookie := SignSession(secret)
	if cookie.Name != "session" {
		t.Errorf("cookie name = %q, want %q", cookie.Name, "session")
	}
	if !cookie.HttpOnly {
		t.Error("cookie should be HttpOnly")
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	if !ValidateSession(req, secret) {
		t.Error("valid session should validate")
	}
}

func TestValidateSessionMissing(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-xxxxx")
	req := httptest.NewRequest("GET", "/", nil)
	if ValidateSession(req, secret) {
		t.Error("missing cookie should not validate")
	}
}

func TestValidateSessionTampered(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-xxxxx")
	cookie := SignSession(secret)
	cookie.Value = "tampered" + cookie.Value
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	if ValidateSession(req, secret) {
		t.Error("tampered cookie should not validate")
	}
}

func TestRequireAdmin(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-xxxxx")
	handler := RequireAdmin(secret, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Without cookie — should redirect to /login
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Errorf("no cookie: status = %d, want %d", w.Code, http.StatusSeeOther)
	}

	// With valid cookie — should pass through
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(SignSession(secret))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("valid cookie: status = %d, want %d", w.Code, http.StatusOK)
	}
}
