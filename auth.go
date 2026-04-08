package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"
)

const sessionCookieName = "session"
const sessionPayload = "authenticated"

func SignSession(secret []byte) *http.Cookie {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sessionPayload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionPayload + "." + sig,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(7 * 24 * time.Hour / time.Second),
	}
}

func ValidateSession(r *http.Request, secret []byte) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	val := cookie.Value
	dot := len(sessionPayload) + 1
	if len(val) <= dot || val[len(sessionPayload)] != '.' {
		return false
	}
	payload := val[:len(sessionPayload)]
	sig := val[dot:]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}

func ClearSession() *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	}
}

func RequireAdmin(secret []byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ValidateSession(r, secret) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
