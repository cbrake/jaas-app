package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseEnv sets the variables every deployment needs, with the JaaS key in
// a file under the name used by earlier releases.
func baseEnv(t *testing.T) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "jaas.pk")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"PUBLIC_ORIGIN":       "https://meet.example.com",
		"JAAS_APP_ID":         testAppID,
		"JAAS_API_KEY_ID":     testAppID + "/key1",
		"JAAS_API_KEY_PATH":   keyPath,
		"JAAS_WEBHOOK_SECRET": testSecret,
	} {
		t.Setenv(k, v)
	}
}

func TestConfigWithoutOpenAIAndLegacyNames(t *testing.T) {
	baseEnv(t)
	t.Setenv("ADMIN_PASSWORD", "legacy-admin-password")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("legacy configuration rejected: %v", err)
	}
	if cfg.JaaSKey == nil || cfg.OpenAIKey != "" || cfg.AdminLegacyPassword != "legacy-admin-password" {
		t.Fatalf("unexpected config: key=%v openai=%q", cfg.JaaSKey != nil, cfg.OpenAIKey)
	}
	if cfg.DisplayTimezone.String() != "UTC" || cfg.MinutesLanguage != "English" || cfg.TranscribeLanguage != "" || cfg.PrivacyURL != "" {
		t.Errorf("defaults: tz=%s lang=%s stt=%q privacy=%q", cfg.DisplayTimezone, cfg.MinutesLanguage, cfg.TranscribeLanguage, cfg.PrivacyURL)
	}

	db, err := InitDB(filepath.Join(t.TempDir(), "jaas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if created, err := db.BootstrapAdmin(cfg.AdminInitialPasswordFile, cfg.AdminLegacyPassword); err != nil || !created {
		t.Fatalf("bootstrap from ADMIN_PASSWORD: %v", err)
	}
	if c, err := db.GetAdminCredential(); err != nil || !c.MustChange {
		t.Fatal("legacy password must be changed on first login")
	}
	if created, _ := db.BootstrapAdmin("", "another-password-value"); created {
		t.Fatal("ADMIN_PASSWORD overwrote the stored credential")
	}
}

func TestConfigValidation(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"openai key without model": {"OPENAI_API_KEY": "sk-test"},
		"bad transcribe language":  {"TRANSCRIBE_LANGUAGE": "english"},
		"privacy url not https":    {"PRIVACY_URL": "http://example.com/privacy"},
		"missing jaas key":         {"JAAS_API_KEY_PATH": ""},
	} {
		t.Run(name, func(t *testing.T) {
			baseEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	baseEnv(t)
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_MODEL", "some-model")
	t.Setenv("TRANSCRIBE_LANGUAGE", "ES")
	t.Setenv("MINUTES_LANGUAGE", "Spanish")
	t.Setenv("PRIVACY_URL", "https://example.com/privacy")
	cfg, err := LoadConfig()
	if err != nil || cfg.TranscribeLanguage != "es" || cfg.MinutesLanguage != "Spanish" || !strings.HasPrefix(cfg.PrivacyURL, "https://") {
		t.Fatalf("valid configuration: %v", err)
	}
}
