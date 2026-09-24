package lan

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var lanHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	},
}

// lanHTTPClient is the shared bounded client for non-fingerprint paths;
// DownloadFileLAN builds its own pinned client per call (see below).

func validateLANURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("LAN URL must use https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("LAN URL missing host")
	}
	// Reject userinfo/credentials and control characters from mDNS.
	if u.User != nil {
		return fmt.Errorf("LAN URL must not contain userinfo")
	}
	if strings.ContainsAny(host, " \t\r\n<>\"'%/\\") && net.ParseIP(host) == nil {
		return fmt.Errorf("invalid LAN host %q", host)
	}
	return nil
}

func DownloadFileLAN(url, dest string, offset int64, expectedFingerprint string, shareCode string, password string, expectedFileSHA256 string, progressCallback func(int64)) error {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // Ephemeral TLS self-signed certificates validation
				VerifyConnection: func(cs tls.ConnectionState) error {
					if len(cs.PeerCertificates) == 0 {
						return fmt.Errorf("no peer certificates presented")
					}
					cert := cs.PeerCertificates[0]
					sum := sha256.Sum256(cert.Raw)
					fingerprint := hex.EncodeToString(sum[:])
					if fingerprint != expectedFingerprint {
						return fmt.Errorf("certificate fingerprint mismatch: expected %s, got %s", expectedFingerprint, fingerprint)
					}
					return nil
				},
			},
		},
	}

	if err := validateLANURL(url); err != nil {
		return fmt.Errorf("invalid LAN URL: %w", err)
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("LAN request: %w", err)
	}

	// Attach security validation headers
	req.Header.Set("X-Uplink-Share-Code", shareCode)
	if password != "" {
		req.Header.Set("X-Uplink-Password", password)
	}

	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("server returned status: %d", resp.StatusCode)
	}
	// If we asked for a range but got a full 200, the server ignored Range:
	// restart from zero like DownloadResumable instead of appending a full
	// body onto the prefix (which would corrupt + delete both).
	if offset > 0 && resp.StatusCode == http.StatusOK {
		offset = 0
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(dest, flags, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	// Compute running SHA-256 hash to protect against file corruption.
	// A reseed failure must fail closed (or restart cleanly), never proceed
	// with a hash missing its prefix and then delete a good partial.
	h := sha256.New()
	if offset > 0 {
		existing, err := os.Open(dest)
		if err != nil {
			return fmt.Errorf("resume reseed open: %w", err)
		}
		if _, err := io.Copy(h, existing); err != nil {
			existing.Close()
			return fmt.Errorf("resume reseed hash: %w", err)
		}
		existing.Close()
	}

	tee := io.TeeReader(resp.Body, h)
	buffer := make([]byte, 32*1024)
	var totalWritten int64
	for {
		nr, readErr := tee.Read(buffer)
		if nr > 0 {
			nw, writeErr := f.Write(buffer[:nr])
			if writeErr != nil {
				return writeErr
			}
			totalWritten += int64(nw)
			if progressCallback != nil {
				progressCallback(totalWritten)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	computedHash := hex.EncodeToString(h.Sum(nil))
	if computedHash != expectedFileSHA256 {
		os.Remove(dest)
		return fmt.Errorf("SHA-256 integrity check failed (expected %s, got %s)", expectedFileSHA256, computedHash)
	}

	return nil
}
