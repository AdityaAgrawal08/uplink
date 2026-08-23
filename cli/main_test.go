package main

import (
	"regexp"
	"testing"
)

func TestGenerateShareCode(t *testing.T) {
	codes := make(map[string]bool)
	alphanumeric := regexp.MustCompile("^[a-zA-Z0-9]{6}$")

	for i := 0; i < 100; i++ {
		code := generateShareCode()
		if len(code) != 6 {
			t.Errorf("expected code length 6, got %d", len(code))
		}
		if !alphanumeric.MatchString(code) {
			t.Errorf("code %q contains non-alphanumeric characters", code)
		}
		if codes[code] {
			t.Errorf("duplicate code generated: %q", code)
		}
		codes[code] = true
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"../../etc/passwd", "passwd"},
		{"folder/file.txt", "file.txt"},
		{"", "file"},
		{".", "file"},
		{"..", "file"},
	}

	for _, tt := range tests {
		got := sanitizeFilename(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeFilename(%q) = %q; expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestParseDurationToSeconds(t *testing.T) {
	tests := []struct {
		input    string
		expected int
		hasError bool
	}{
		{"5m", 300, false},
		{"2h", 7200, false},
		{"1d", 86400, false},
		{"invalid", 0, true},
		{"0h", 0, true},
	}

	for _, tt := range tests {
		got, err := parseDurationToSeconds(tt.input)
		if (err != nil) != tt.hasError {
			t.Errorf("parseDurationToSeconds(%q) error = %v; expected error = %v", tt.input, err, tt.hasError)
		}
		if !tt.hasError && got != tt.expected {
			t.Errorf("parseDurationToSeconds(%q) = %d; expected %d", tt.input, got, tt.expected)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}

	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.expected {
			t.Errorf("formatBytes(%d) = %q; expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestChunkSizeFloor(t *testing.T) {
	ac := &AdaptiveChunker{}
	// Simulate a very slow link (2 KB/s) — the measured chunk would be ~10 KB.
	ac.RecordSpeed(2048)
	cs := ac.ChunkSize()
	if cs != minChunk {
		t.Errorf("slow-link chunk size = %d; want floor %d (5 MiB, S3 minimum part size)", cs, minChunk)
	}
	if cs < 5<<20 {
		t.Errorf("chunk size %d below S3 5 MiB part minimum", cs)
	}

	fast := &AdaptiveChunker{}
	fast.RecordSpeed(100 << 20) // 100 MB/s → clamps to max
	if got := fast.ChunkSize(); got != maxChunk {
		t.Errorf("fast-link chunk size = %d; want ceiling %d", got, maxChunk)
	}
}

func TestNormalizeFlagOrder(t *testing.T) {
	valueFlags := map[string]bool{"server": true, "password": true, "expire": true}

	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "flags already first are preserved",
			input:    []string{"--server", "http://x", "file.txt"},
			expected: []string{"--server", "http://x", "file.txt"},
		},
		{
			name:     "flags after positional move to front",
			input:    []string{"file.txt", "--no-qr", "--server", "http://x"},
			expected: []string{"--no-qr", "--server", "http://x", "file.txt"},
		},
		{
			name:     "equals-form value flag stays intact",
			input:    []string{"file.txt", "--server=http://y", "-f"},
			expected: []string{"--server=http://y", "-f", "file.txt"},
		},
		{
			name:     "double dash terminator keeps remainder positional",
			input:    []string{"--no-qr", "--", "-weird.txt", "--server"},
			expected: []string{"--no-qr", "-weird.txt", "--server"},
		},
		{
			name:     "value flag at end without value",
			input:    []string{"file.txt", "--password"},
			expected: []string{"--password", "file.txt"},
		},
		{
			name:     "mixed shortcuts and long flags",
			input:    []string{"4827165038", "-r", "--server", "http://z", "destDir"},
			expected: []string{"-r", "--server", "http://z", "4827165038", "destDir"},
		},
	}

	for _, tt := range tests {
		got := normalizeFlagOrder(tt.input, valueFlags)
		if len(got) != len(tt.expected) {
			t.Errorf("%s: normalizeFlagOrder(%v) = %v; expected %v", tt.name, tt.input, got, tt.expected)
			continue
		}
		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("%s: normalizeFlagOrder(%v) = %v; expected %v", tt.name, tt.input, got, tt.expected)
				break
			}
		}
	}
}
