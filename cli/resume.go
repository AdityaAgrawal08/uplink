package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type ResumeState struct {
	ShareId    string     `json:"shareId"`
	UploadId   string     `json:"uploadId"`
	UploadUrls []string   `json:"uploadUrls"`
	FileSize   int64      `json:"size"`
	SHA256     string     `json:"sha256"`
	Done       []int      `json:"done"`
	Parts      []PartInfo `json:"parts,omitempty"`
	TotalParts int        `json:"total"`
	ChunkSize  int64      `json:"chunkSize,omitempty"`
	Timestamp  string     `json:"ts"`
}

func getResumeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".uplink", "resume")
}

func (s *ResumeState) Save(filename string) error {
	dir := getResumeDir()
	err := os.MkdirAll(dir, 0700)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, filename+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	err = json.NewEncoder(f).Encode(s)
	if err != nil {
		f.Close()
		return err
	}
	// Sync before rename so a crash cannot leave a valid-named but
	// truncated state file.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, filename))
}

func LoadResumeState(filename string) (*ResumeState, error) {
	path := filepath.Join(getResumeDir(), filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state ResumeState
	err = json.Unmarshal(data, &state)
	if err != nil {
		return nil, err
	}
	return &state, nil
}

func DeleteResumeState(filename string) error {
	return os.Remove(filepath.Join(getResumeDir(), filename))
}

func (s *ResumeState) Valid() bool {
	if s.TotalParts <= 0 {
		return false
	}
	t, err := time.Parse(time.RFC3339, s.Timestamp)
	if err != nil {
		return false
	}
	// Expire after 2 hours (S3 presigned URL expiration)
	if time.Since(t) >= 2*time.Hour {
		return false
	}
	// All parts done but confirm never ran (crash after final part):
	// still resumable — caller skips straight to confirm.
	if len(s.Done) >= s.TotalParts {
		return true
	}
	return true
}

// ComputeSHA256Stream computes the SHA-256 hash of a file using a streaming approach
func ComputeSHA256Stream(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func CleanOldResumeStates() {
	dir := getResumeDir()
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".json" {
			path := filepath.Join(dir, file.Name())
			if info, err := file.Info(); err == nil {
				if time.Since(info.ModTime()) > 24*time.Hour {
					_ = os.Remove(path)
				}
			}
		}
	}
}
