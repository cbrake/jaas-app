package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

type Config struct {
	JaaSAppID     string
	JaaSKeyID     string
	JaaSKey       *rsa.PrivateKey
	AdminPassword string
	SessionSecret []byte
	ListenAddr    string
	DBPath        string
}

func LoadConfig() (*Config, error) {
	appID := os.Getenv("JAAS_APP_ID")
	if appID == "" {
		return nil, fmt.Errorf("JAAS_APP_ID is required")
	}

	keyID := os.Getenv("JAAS_API_KEY_ID")
	if keyID == "" {
		return nil, fmt.Errorf("JAAS_API_KEY_ID is required")
	}

	// The key may be supplied inline (convenient for container platforms that
	// only offer environment secrets) or as a path to a PEM file.
	keyData := []byte(os.Getenv("JAAS_API_KEY"))
	if len(keyData) == 0 {
		keyPath := os.Getenv("JAAS_API_KEY_PATH")
		if keyPath == "" {
			return nil, fmt.Errorf("JAAS_API_KEY or JAAS_API_KEY_PATH is required")
		}
		var err error
		keyData, err = os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("reading JAAS_API_KEY_PATH: %w", err)
		}
	}

	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in the JaaS API key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS1 as fallback
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing private key: %w", err)
		}
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA")
	}

	adminPass := os.Getenv("ADMIN_PASSWORD")
	if adminPass == "" {
		return nil, fmt.Errorf("ADMIN_PASSWORD is required")
	}

	sessionSecret := os.Getenv("SESSION_SECRET")
	if sessionSecret == "" {
		return nil, fmt.Errorf("SESSION_SECRET is required")
	}

	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8370"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./jaas.db"
	}

	return &Config{
		JaaSAppID:     appID,
		JaaSKeyID:     keyID,
		JaaSKey:       rsaKey,
		AdminPassword: adminPass,
		SessionSecret: []byte(sessionSecret),
		ListenAddr:    listenAddr,
		DBPath:        dbPath,
	}, nil
}
