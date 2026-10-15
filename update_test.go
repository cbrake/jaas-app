package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleaseAssetNameMatchesGoreleaser(t *testing.T) {
	// Names published for v0.1.0 (see .goreleaser.yaml archives.name_template).
	for _, c := range []struct{ goos, goarch, goarm, want string }{
		{"linux", "amd64", "", "jaas-app-v0.1.0-Linux-x86_64"},
		{"linux", "arm64", "", "jaas-app-v0.1.0-Linux-arm64"},
		{"linux", "arm", "", "jaas-app-v0.1.0-Linux-armv6"},
		{"linux", "riscv64", "", "jaas-app-v0.1.0-Linux-riscv64"},
		{"darwin", "arm64", "", "jaas-app-v0.1.0-Macos-arm64"},
		{"windows", "amd64", "", "jaas-app-v0.1.0-Windows-x86_64.exe"},
	} {
		if got := releaseAssetName("v0.1.0", c.goos, c.goarch, c.goarm); got != c.want {
			t.Errorf("%s/%s: got %s, want %s", c.goos, c.goarch, got, c.want)
		}
	}
	if checksumsName("v0.1.0") != "jaas-app_0.1.0_checksums.txt" {
		t.Error(checksumsName("v0.1.0"))
	}
}

func TestFetchVerifiedRelease(t *testing.T) {
	binary := []byte("new binary")
	sum := sha256.Sum256(binary)
	name := "jaas-app-v1.2.3-Linux-x86_64"
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n" + strings.Repeat("0", 64) + "  jaas-app-v1.2.3-Macos-arm64\n"
	served := binary
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.2.3/jaas-app_1.2.3_checksums.txt":
			w.Write([]byte(sums))
		case "/v1.2.3/" + name:
			w.Write(served)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := fetchVerifiedRelease(srv.URL+"/v1.2.3", "v1.2.3", name)
	if err != nil || string(got) != string(binary) {
		t.Fatalf("valid release rejected: %v", err)
	}
	served = []byte("tampered binary")
	if _, err := fetchVerifiedRelease(srv.URL+"/v1.2.3", "v1.2.3", name); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered binary accepted: %v", err)
	}
	if _, err := fetchVerifiedRelease(srv.URL+"/v1.2.3", "v1.2.3", "jaas-app-v1.2.3-Linux-arm64"); err == nil {
		t.Fatal("binary without a published checksum accepted")
	}
}
