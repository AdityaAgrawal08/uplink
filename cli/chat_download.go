package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// ---- received files (/download drawer source) --------------------------------
//
// There is no server-side file index anymore: files arrive over the direct
// line (or the inbox fallback) and land straight in ~/Downloads via the
// engine. The /download drawer therefore lists files received this session
// (chatScreen.received), newest first, with their save locations.

// downloadsDir resolves ~/Downloads (created on demand).
func downloadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	dir := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// uniquePath avoids clobbering: report.pdf -> report (1).pdf -> report (2).pdf…
// Atomic-claim variant available via claimUniquePath (O_EXCL).
func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return p // free
	}
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	for i := 1; i <= 10000; i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(p); err != nil {
			return p
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, 10000, ext))
}

// claimUniquePath atomically claims a free path with O_EXCL and returns the
// open file (0600). Prevents TOCTOU where two savers Stat the same free name
// and one Rename clobbers the other.
func claimUniquePath(dir, name string, maxTries int) (*os.File, string, error) {
	if maxTries <= 0 || maxTries > 10000 {
		maxTries = 1000
	}
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	candidates := make([]string, 0, maxTries+1)
	candidates = append(candidates, filepath.Join(dir, name))
	for i := 1; i <= maxTries; i++ {
		candidates = append(candidates, filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext)))
	}
	for _, p := range candidates {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			return f, p, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("no free filename for %s", name)
}
