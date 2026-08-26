package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// version is set via ldflags at build time: go build -ldflags "-X main.version=1.2.3"
var version = "0.0.1"

func handleVersion() {
	fmt.Printf("uplink %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
}

func handleUpdate() {
	fmt.Println("Checking for latest release...")
	ghToken := os.Getenv("GITHUB_TOKEN")
	url := "https://api.github.com/repos/AdityaAgrawal08/uplink-delta/releases/latest"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Printf("✗ Failed to build request: %v\n", err)
		os.Exit(1)
	}
	if ghToken != "" {
		req.Header.Set("Authorization", "Bearer "+ghToken)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("✗ Failed to check for updates: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("✗ GitHub API returned status %d: %s\n", resp.StatusCode, truncateStringPlain(string(body), 120))
		os.Exit(1)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		fmt.Printf("✗ Failed to parse response: %v\n", err)
		os.Exit(1)
	}

	latestTag := strings.TrimPrefix(release.TagName, "v")
	if latestTag == version {
		fmt.Printf("✓ Already up to date (v%s)\n", version)
		return
	}

	fmt.Printf("Update available: v%s → v%s\n", version, latestTag)

	// Find matching asset
	suffix := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, a := range release.Assets {
		name := strings.ToLower(a.Name)
		if strings.Contains(name, suffix) || (runtime.GOOS == "darwin" && strings.Contains(name, "darwin")) {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}

	if downloadURL == "" {
		fmt.Println("✗ No compatible binary found for your platform")
		fmt.Printf("  Download manually: https://github.com/AdityaAgrawal08/uplink-delta/releases/tag/%s\n", release.TagName)
		os.Exit(1)
	}

	fmt.Printf("Downloading %s...\n", filepath.Base(downloadURL))
	tmpFile, err := os.CreateTemp("", "uplink-update-*")
	if err != nil {
		fmt.Printf("✗ Temp file creation failed: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmpFile.Name())

	dlResp, err := client.Get(downloadURL)
	if err != nil {
		fmt.Printf("✗ Download failed: %v\n", err)
		os.Exit(1)
	}
	defer dlResp.Body.Close()

	if dlResp.StatusCode != 200 {
		fmt.Printf("✗ Download returned status %d\n", dlResp.StatusCode)
		os.Exit(1)
	}

	if _, err := io.Copy(tmpFile, dlResp.Body); err != nil {
		fmt.Printf("✗ Download interrupted: %v\n", err)
		os.Exit(1)
	}
	tmpFile.Close()

	// Determine install path
	selfPath, err := exec.LookPath("uplink")
	if err != nil {
		selfPath = filepath.Join(".", "uplink")
	}

	if err := os.Chmod(tmpFile.Name(), 0755); err != nil {
		fmt.Printf("✗ Cannot make binary executable: %v\n", err)
		os.Exit(1)
	}

	if err := os.Rename(tmpFile.Name(), selfPath); err != nil {
		// Try copy if rename fails (cross-device)
		in, err := os.Open(tmpFile.Name())
		if err != nil {
			fmt.Printf("✗ Cannot read new binary: %v\n", err)
			os.Exit(1)
		}
		defer in.Close()
		out, err := os.OpenFile(selfPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
		if err != nil {
			fmt.Printf("✗ Cannot write to %s: %v\n", selfPath, err)
			os.Exit(1)
		}
		defer out.Close()
		if _, err := io.Copy(out, in); err != nil {
			fmt.Printf("✗ Install failed: %v\n", err)
			os.Exit(1)
		}
		os.Remove(tmpFile.Name())
	}

	fmt.Printf("✓ Updated to v%s (%s)\n", latestTag, selfPath)
}
