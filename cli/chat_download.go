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
func uniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return p // free
	}
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	for i := 1; ; i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(p); err != nil {
			return p
		}
	}
}
