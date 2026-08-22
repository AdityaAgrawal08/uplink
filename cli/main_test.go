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
