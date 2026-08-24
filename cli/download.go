package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func readPartialMetadata(sha256Path string) (int64, string, error) {
	data, err := os.ReadFile(sha256Path)
	if err != nil {
		return 0, "", err
	}
	parts := strings.Split(strings.TrimSpace(string(data)), ":")
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("invalid partial metadata format")
	}
	offset, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("invalid offset in metadata: %v", err)
	}
	return offset, parts[1], nil
}

func writePartialMetadata(sha256Path string, offset int64, hashHex string) error {
	content := fmt.Sprintf("%d:%s", offset, hashHex)
	return os.WriteFile(sha256Path, []byte(content), 0644)
}

func DownloadResumable(url, dest string, expectedHash string, progressPrinter func(int64, int64)) error {
	partialFile := dest + ".uplink.partial"
	metadataFile := partialFile + ".sha256"

	var offset int64 = 0

	// Check if metadata and partial file exist and match
	if _, err := os.Stat(partialFile); err == nil {
		if _, err := os.Stat(metadataFile); err == nil {
			metaOffset, savedHash, err := readPartialMetadata(metadataFile)
			if err == nil {
				stat, err := os.Stat(partialFile)
				if err == nil && stat.Size() >= metaOffset {
					if stat.Size() > metaOffset {
						_ = os.Truncate(partialFile, metaOffset)
					}

					h := sha256.New()
					pf, err := os.Open(partialFile)
					if err == nil {
						_, _ = io.CopyN(h, pf, metaOffset)
						pf.Close()
						computedHash := hex.EncodeToString(h.Sum(nil))
						if computedHash == savedHash {
							offset = metaOffset
							fmt.Printf("Resuming download from offset %s...\n", formatBytes(offset))
						}
					}
				}
			}
		}
	}

	if offset == 0 {
		_ = os.Remove(partialFile)
		_ = os.Remove(metadataFile)
	}

	req, _ := http.NewRequest("GET", url, nil)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download start: %w", err)
	}
	defer resp.Body.Close()

	if offset > 0 && resp.StatusCode == http.StatusOK {
		offset = 0
		_ = os.Remove(partialFile)
		_ = os.Remove(metadataFile)
	}

	f, err := os.OpenFile(partialFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	runningHasher := sha256.New()
	if offset > 0 {
		pf, err := os.Open(partialFile)
		if err == nil {
			_, _ = io.Copy(runningHasher, pf)
			pf.Close()
		}
	}

	buffer := make([]byte, 32*1024)
	var lastCheckpoint int64 = offset
	var totalWritten int64 = offset

	for {
		nr, readErr := resp.Body.Read(buffer)
		if nr > 0 {
			nw, writeErr := f.Write(buffer[:nr])
			if writeErr != nil {
				return writeErr
			}
			runningHasher.Write(buffer[:nw])
			totalWritten += int64(nw)

			if progressPrinter != nil {
				progressPrinter(totalWritten, offset)
			}

			// Periodically save checkpoints (every 10 MB)
			if totalWritten-lastCheckpoint >= 10*1024*1024 {
				_ = f.Sync()
				currentHash := hex.EncodeToString(runningHasher.Sum(nil))
				_ = writePartialMetadata(metadataFile, totalWritten, currentHash)
				lastCheckpoint = totalWritten
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(partialFile)
		_ = os.Remove(metadataFile)
		return fmt.Errorf("failed to close file: %w", err)
	}

	finalHash := hex.EncodeToString(runningHasher.Sum(nil))
	if finalHash != expectedHash {
		// Double check by reading full file
		computed, err := ComputeSHA256Stream(partialFile)
		if err != nil || computed != expectedHash {
			_ = os.Remove(partialFile)
			_ = os.Remove(metadataFile)
			if err != nil {
				return fmt.Errorf("SHA-256 verification error: %w", err)
			}
			return fmt.Errorf("SHA-256 verification failed (expected %s, got %s)", expectedHash, finalHash)
		}
	}

	_ = os.Remove(metadataFile)
	return os.Rename(partialFile, dest)
}
