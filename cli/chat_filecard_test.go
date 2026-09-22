package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)


func TestTruncateFilenameRuneSafe(t *testing.T) {
	name := strings.Repeat("😀", 40) + ".png"
	got := truncateFilename(name, 20)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
	if len([]rune(got)) > 20 {
		t.Fatalf("truncation exceeded maxLen in runes: %q", got)
	}
	cjk := strings.Repeat("漢", 30) + ".txt"
	got = truncateFilename(cjk, 12)
	if !utf8.ValidString(got) || len([]rune(got)) > 12 {
		t.Fatalf("CJK truncation invalid: %q", got)
	}
	// Short names pass through untouched.
	if got := truncateFilename("ok.txt", 20); got != "ok.txt" {
		t.Fatalf("short name altered: %q", got)
	}
}
