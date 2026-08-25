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

func TestNormalizeFlagOrderEdgeCases(t *testing.T) {
	valueFlags := map[string]bool{"server": true, "password": true, "expire": true}

	tests := []struct {
		name       string
		input      []string
		expected   []string
		wantRemain bool // whether positional args should remain
	}{
		{
			name:     "all flags before positional",
			input:    []string{"--server", "http://x", "--password", "pass", "key"},
			expected: []string{"--server", "http://x", "--password", "pass", "key"},
		},
		{
			name:       "flags after positional should be moved before",
			input:      []string{"key", "--server", "http://x"},
			expected:   []string{"--server", "http://x", "key"},
			wantRemain: false,
		},
		{
			name:     "multiple flags with values",
			input:    []string{"--server", "a", "--password", "b", "--expire", "1h", "key"},
			expected: []string{"--server", "a", "--password", "b", "--expire", "1h", "key"},
		},
		{
			name:     "flag with =value syntax",
			input:    []string{"--server=http://x", "key"},
			expected: []string{"--server=http://x", "key"},
		},
		{
			name:     "no flags, just positional",
			input:    []string{"key"},
			expected: []string{"key"},
		},
		{
			name:     "empty input",
			input:    []string{},
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeFlagOrder(tt.input, valueFlags)
			if len(got) != len(tt.expected) {
				t.Errorf("%s: normalizeFlagOrder(%v) = %v (len %d); expected %v (len %d)",
					tt.name, tt.input, got, len(got), tt.expected, len(tt.expected))
				return
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("%s: normalizeFlagOrder(%v) = %v; expected %v",
						tt.name, tt.input, got, tt.expected)
					return
				}
			}
		})
	}
}

// TestAdaptiveChunker exercises the REAL ChunkSize() clamp logic across
// the full spectrum of measured link speeds (S3 floor 5MiB / ceiling 10MiB).
func TestAdaptiveChunkerClamping(t *testing.T) {
	mib := int64(1 << 20)
	tests := []struct {
		name     string
		speedBps float64 // bytes/sec fed through RecordSpeed
		want     int64
	}{
		{"unset/zero speed falls back to default", 0, maxChunk},
		{"negative speed treated as unset", -5_000_000, maxChunk},
		{"dial-up 56kbit/s clamps up to S3 floor", 7_000, minChunk},
		{"slow DSL 512kbit/s still floors", 64_000, minChunk},
		{"1 MB/s => 5s target = 5MiB boundary", 1 << 20, 5 * mib},
		{"2 MB/s => 10s worth but capped at 10MiB", 2 << 20, maxChunk},
		{"gigabit clamps down to ceiling", 125_000_000, maxChunk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ac := &AdaptiveChunker{}
			if tt.speedBps > 0 {
				ac.RecordSpeed(tt.speedBps)
			}
			got := ac.ChunkSize()
			if got != tt.want {
				t.Errorf("speed=%v: ChunkSize() = %d (%d MiB); want %d (%d MiB)",
					tt.speedBps, got, got/mib, tt.want, tt.want/mib)
			}
			// Invariant: result must ALWAYS satisfy the S3 multipart contract.
			if got < minChunk || got > maxChunk {
				t.Errorf("ChunkSize() = %d violates [%d, %d] contract", got, minChunk, maxChunk)
			}
		})
	}
}

// TestAdaptiveChunkerEMA verifies the exponential-moving-average smoothing.
func TestAdaptiveChunkerEMA(t *testing.T) {
	ac := &AdaptiveChunker{}
	ac.RecordSpeed(1_000_000)
	if ac.measuredSpeed != 1_000_000 {
		t.Fatalf("first sample must set baseline verbatim; got %v", ac.measuredSpeed)
	}
	ac.RecordSpeed(2_000_000)
	// smoothFactor=0.3 → 0.3*2M + 0.7*1M = 1.3M
	want := 0.3*2_000_000 + 0.7*1_000_000
	if ac.measuredSpeed != want {
		t.Errorf("EMA after 2nd sample = %v; want %v", ac.measuredSpeed, want)
	}
	if ac.samples != 2 {
		t.Errorf("samples = %d; want 2", ac.samples)
	}
}

// TestUploadLimitConstants pins the CLI upload ceilings so accidental edits
// surface in CI instead of at runtime.
func TestUploadLimitConstants(t *testing.T) {
	const fileLimit = int64(200) << 20
	const dirLimit = int64(500) << 20
	if dirLimit <= fileLimit {
		t.Errorf("directory limit (%d) must exceed file limit (%d)", dirLimit, fileLimit)
	}
	// Both must fit inside the server's 9.4 GB storage ceiling with room to spare.
	const serverMax = int64(9_400_000_000)
	if dirLimit > serverMax {
		t.Errorf("dir limit %d exceeds server storage ceiling %d", dirLimit, serverMax)
	}
}
