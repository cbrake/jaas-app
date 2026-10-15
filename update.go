package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

const (
	githubAPIURL      = "https://api.github.com/repos/cbrake/jaas-app/releases/latest"
	githubDownloadURL = "https://github.com/cbrake/jaas-app/releases/download"

	maxReleaseBinary = 256 << 20
	maxChecksumsFile = 1 << 20
)

var updateClient = &http.Client{Timeout: 5 * time.Minute}

// GitHubRelease represents the GitHub API release response
type GitHubRelease struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
}

// Update checks for and downloads the latest version of jaas-app. The binary
// is installed only if its SHA-256 matches the checksums file goreleaser
// publishes with the same release (security.md L2).
func Update(currentVersion string) error {
	fmt.Println("Checking for updates...")

	// Get latest release info from GitHub API
	latestVersion, err := getLatestVersion()
	if err != nil {
		return fmt.Errorf("failed to check for updates: %w", err)
	}

	// Normalize versions for comparison (remove 'v' prefix)
	current := strings.TrimPrefix(currentVersion, "v")
	latest := strings.TrimPrefix(latestVersion, "v")

	if current == latest {
		fmt.Printf("Already running the latest version (%s)\n", currentVersion)
		return nil
	}

	if current == "dev" {
		fmt.Printf("Running development version. Latest release is %s\n", latestVersion)
		fmt.Println("Proceeding with update...")
	} else {
		fmt.Printf("Updating from %s to %s\n", currentVersion, latestVersion)
	}

	// Download, verify and install the latest version
	if err := downloadAndInstall(latestVersion); err != nil {
		return fmt.Errorf("failed to update: %w", err)
	}

	fmt.Printf("Successfully updated to version %s\n", latestVersion)
	return nil
}

// getLatestVersion fetches the latest release version from GitHub
func getLatestVersion() (string, error) {
	resp, err := updateClient.Get(githubAPIURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release GitHubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return "", err
	}
	if release.TagName == "" || strings.ContainsAny(release.TagName, "/\\?#") {
		return "", errors.New("unexpected release tag")
	}

	return release.TagName, nil
}

// downloadAndInstall downloads the release binary for this platform, checks
// it against the release's checksums file and replaces the running binary.
func downloadAndInstall(version string) error {
	binaryName := getBinaryName(version)
	fmt.Printf("Downloading %s...\n", binaryName)
	data, err := fetchVerifiedRelease(githubDownloadURL+"/"+version, version, binaryName)
	if err != nil {
		return err
	}
	fmt.Println("Checksum verified")

	// Get the current executable path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("failed to resolve symlinks: %w", err)
	}

	// Create a temporary file
	tmpFile, err := os.CreateTemp(filepath.Dir(execPath), "jaas-app-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	// Write the verified binary to the temp file
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write binary: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to write binary: %w", err)
	}

	// Make the temp file executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("failed to make binary executable: %w", err)
	}

	// Replace the current binary with the new one
	if runtime.GOOS == "windows" {
		oldPath := execPath + ".old"
		os.Remove(oldPath)
		if err := os.Rename(execPath, oldPath); err != nil {
			return fmt.Errorf("failed to backup old binary: %w", err)
		}
		if err := os.Rename(tmpPath, execPath); err != nil {
			os.Rename(oldPath, execPath)
			return fmt.Errorf("failed to install new binary: %w", err)
		}
		os.Remove(oldPath)
	} else {
		if err := os.Rename(tmpPath, execPath); err != nil {
			return fmt.Errorf("failed to install new binary: %w", err)
		}
	}

	fmt.Println("Binary updated successfully")
	return nil
}

// fetchVerifiedRelease downloads binaryName from the release at base and
// returns it only if its SHA-256 matches the release's checksums file.
func fetchVerifiedRelease(base, version, binaryName string) ([]byte, error) {
	sums, err := fetchLimited(base+"/"+checksumsName(version), maxChecksumsFile)
	if err != nil {
		return nil, fmt.Errorf("failed to download checksums: %w", err)
	}
	want, err := checksumFor(sums, binaryName)
	if err != nil {
		return nil, err
	}
	data, err := fetchLimited(base+"/"+binaryName, maxReleaseBinary)
	if err != nil {
		return nil, fmt.Errorf("failed to download: %w", err)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s: refusing to install", binaryName)
	}
	return data, nil
}

func fetchLimited(url string, limit int64) ([]byte, error) {
	resp, err := updateClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file too large")
	}
	return data, nil
}

// checksumsName is goreleaser's default checksum file name for a release.
func checksumsName(version string) string {
	return fmt.Sprintf("jaas-app_%s_checksums.txt", strings.TrimPrefix(version, "v"))
}

// checksumFor returns the lowercase hex SHA-256 listed for name in a
// sha256sum-style file ("<hex>  <name>" per line).
func checksumFor(sums []byte, name string) (string, error) {
	for _, line := range bytes.Split(sums, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) != 2 || f[1] != name {
			continue
		}
		sum := strings.ToLower(f[0])
		if _, err := hex.DecodeString(sum); err != nil || len(sum) != 64 {
			return "", fmt.Errorf("malformed checksum for %s", name)
		}
		return sum, nil
	}
	return "", fmt.Errorf("no checksum published for %s", name)
}

// getBinaryName returns the release asset name for the current platform. It
// follows the archives.name_template in .goreleaser.yaml, e.g.
// jaas-app-v0.1.0-Linux-x86_64, jaas-app-v0.1.0-Macos-arm64,
// jaas-app-v0.1.0-Linux-armv6, jaas-app-v0.1.0-Windows-x86_64.exe.
func getBinaryName(version string) string {
	return releaseAssetName(version, runtime.GOOS, runtime.GOARCH, buildGOARM())
}

func releaseAssetName(version, goos, goarch, goarm string) string {
	osName := strings.ToUpper(goos[:1]) + goos[1:]
	if goos == "darwin" {
		osName = "Macos"
	}
	archName := goarch
	if goarch == "amd64" {
		archName = "x86_64"
	}
	if goarch == "arm" {
		if goarm == "" {
			goarm = "6" // goreleaser's default
		}
		archName += "v" + goarm
	}
	name := fmt.Sprintf("jaas-app-%s-%s-%s", version, osName, archName)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// buildGOARM reports the GOARM the running binary was built with.
func buildGOARM() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "GOARM" {
				return strings.SplitN(s.Value, ",", 2)[0]
			}
		}
	}
	return ""
}
