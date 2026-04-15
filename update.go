package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	githubAPIURL = "https://api.github.com/repos/cbrake/jaas-app/releases/latest"
	githubRelURL = "https://github.com/cbrake/jaas-app/releases/latest/download"
)

// GitHubRelease represents the GitHub API release response
type GitHubRelease struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
}

// Update checks for and downloads the latest version of jaas-app
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

	// Download and install the latest version
	if err := downloadAndInstall(latestVersion); err != nil {
		return fmt.Errorf("failed to update: %w", err)
	}

	fmt.Printf("Successfully updated to version %s\n", latestVersion)
	return nil
}

// getLatestVersion fetches the latest release version from GitHub
func getLatestVersion() (string, error) {
	resp, err := http.Get(githubAPIURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	return release.TagName, nil
}

// downloadAndInstall downloads and installs the latest version
func downloadAndInstall(version string) error {
	// Determine the binary name based on OS and architecture
	binaryName := getBinaryName(version)
	downloadURL := fmt.Sprintf("%s/%s", githubRelURL, binaryName)

	fmt.Printf("Downloading %s...\n", downloadURL)

	// Download the binary
	resp, err := http.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

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

	// Write the downloaded binary to the temp file
	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write binary: %w", err)
	}
	tmpFile.Close()

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

// getBinaryName returns the binary name for the current platform
func getBinaryName(version string) string {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	osName := goos
	if goos == "darwin" {
		osName = "macos"
	}

	archName := goarch
	if goarch == "amd64" {
		archName = "x86_64"
	} else if goarch == "386" {
		archName = "i386"
	}

	armVersion := ""
	if goarch == "arm" {
		armVersion = "v7"
		if v := os.Getenv("GOARM"); v != "" {
			armVersion = "v" + v
		}
	}

	return fmt.Sprintf("jaas-app-%s-%s-%s%s", version, osName, archName, armVersion)
}
