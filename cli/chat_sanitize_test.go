package main

import (
	"strings"
	"testing"
)

func TestSanitizeDisplayStripsEscapes(t *testing.T) {
	attacks := []string{
		"hi\x1b[2Jbye",          // clear screen
		"\x1b]0;pwned\a",        // set title (OSC)
		"text\x1b[8mhiddentext", // conceal
		"\x1b[31mred",           // SGR color
		"a\x07b",                // BEL
		"a\x00b\x1fc",           // NUL + control
		"a\x7fb",                // DEL
	}
	for _, in := range attacks {
		out := sanitizeDisplay(in)
		if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) {
			t.Fatalf("escape survived in %q -> %q", in, out)
		}
	}
	// Legit layout characters survive.
	if got := sanitizeDisplay("line one\nline\ttwo"); got != "line one\nline\ttwo" {
		t.Fatalf("newlines/tabs must survive: %q", got)
	}
	if got := sanitizeDisplay("héllo 世界 🚀 **bold**"); got != "héllo 世界 🚀 **bold**" {
		t.Fatalf("unicode/markdown must survive: %q", got)
	}
	if sanitizeDisplay("") != "" {
		t.Fatal("empty must stay empty")
	}
}

func TestRenderMarkdownNeutralizesTerminalAttacks(t *testing.T) {
	out := renderMarkdown("**hi**\x1b[2J")
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("ESC reached the terminal: %q", out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("content lost: %q", out)
	}
}
