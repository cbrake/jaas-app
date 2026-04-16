package main

import (
	"crypto/rsa"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func GenerateJWT(key *rsa.PrivateKey, appID, keyID, room, userName string, moderator, transcription bool) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"aud":  "jitsi",
		"iss":  "chat",
		"sub":  appID,
		"room": room,
		"exp":  jwt.NewNumericDate(now.Add(4 * time.Hour)),
		"nbf":  jwt.NewNumericDate(now),
		"context": map[string]any{
			"user": map[string]any{
				"name":      userName,
				"moderator": moderator,
			},
			"features": map[string]any{
				"recording":     moderator,
				"livestreaming": false,
				"transcription": transcription,
			},
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = keyID
	return token.SignedString(key)
}
