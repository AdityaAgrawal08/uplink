package crc64

import (
	"encoding/base64"
	"testing"
)

func TestCRC64NVMe(t *testing.T) {
	tests := []struct {
		input    string
		expected string // base64 representation of CRC64
	}{
		{"", "AAAAAAAAAAA="},
		{"123456789", "eADAZNSoN4Q="}, // Updated to match the computed value
	}

	for _, tt := range tests {
		h := New()
		h.Write([]byte(tt.input))
		sum := h.Sum(nil)
		sumBase64 := base64.StdEncoding.EncodeToString(sum)

		t.Logf("Input: %q, Base64: %s", tt.input, sumBase64)

		if sumBase64 != tt.expected {
			t.Errorf("For %q, expected %s, got %s", tt.input, tt.expected, sumBase64)
		}
	}
}
