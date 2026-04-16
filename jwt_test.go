package main

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func TestGenerateGuestJWT(t *testing.T) {
	key := testKey(t)
	appID := "vpaas-magic-cookie-test123"
	keyID := "vpaas-magic-cookie-test123/mykey"
	tokenStr, err := GenerateJWT(key, appID, keyID, "weekly-sync", "Alice", false, false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	token, err := jwtlib.Parse(tokenStr, func(token *jwtlib.Token) (any, error) {
		return &key.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !token.Valid {
		t.Fatal("token should be valid")
	}
	claims := token.Claims.(jwtlib.MapClaims)
	if claims["aud"] != "jitsi" {
		t.Errorf("aud = %v, want jitsi", claims["aud"])
	}
	if claims["iss"] != "chat" {
		t.Errorf("iss = %v, want chat", claims["iss"])
	}
	if claims["sub"] != appID {
		t.Errorf("sub = %v, want %s", claims["sub"], appID)
	}
	if claims["room"] != "weekly-sync" {
		t.Errorf("room = %v, want weekly-sync", claims["room"])
	}
	ctx := claims["context"].(map[string]any)
	user := ctx["user"].(map[string]any)
	if user["name"] != "Alice" {
		t.Errorf("name = %v, want Alice", user["name"])
	}
	if user["moderator"] != false {
		t.Error("guest should not be moderator")
	}
	features := ctx["features"].(map[string]any)
	if features["recording"] != false {
		t.Error("guest should not have recording")
	}
	if token.Header["kid"] != keyID {
		t.Errorf("kid = %v, want %s", token.Header["kid"], keyID)
	}
}

func TestGenerateModeratorJWT(t *testing.T) {
	key := testKey(t)
	appID := "vpaas-magic-cookie-test123"
	keyID := "vpaas-magic-cookie-test123/mykey"
	tokenStr, err := GenerateJWT(key, appID, keyID, "weekly-sync", "Bob", true, true)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	token, _ := jwtlib.Parse(tokenStr, func(token *jwtlib.Token) (any, error) {
		return &key.PublicKey, nil
	})
	claims := token.Claims.(jwtlib.MapClaims)
	ctx := claims["context"].(map[string]any)
	user := ctx["user"].(map[string]any)
	if user["moderator"] != true {
		t.Error("moderator should be true")
	}
	features := ctx["features"].(map[string]any)
	if features["recording"] != true {
		t.Error("moderator should have recording")
	}
}
