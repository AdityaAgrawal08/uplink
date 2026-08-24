package lan

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
)

func DownloadFileLAN(url, dest string, offset int64, expectedFingerprint string, shareCode string, password string, expectedFileSHA256 string, progressCallback func(int64)) error {
	tr := &http.Transport{
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
	}
	client := &http.Client{Transport: tr}

	req, _ := http.NewRequest("GET", url, nil)

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

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(dest, flags, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Compute running SHA-256 hash to protect against file corruption
	h := sha256.New()
	if offset > 0 {
		existing, err := os.Open(dest)
		if err == nil {
			_, _ = io.Copy(h, existing)
			existing.Close()
		}
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
