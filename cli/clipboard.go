package main

import (
	"github.com/atotto/clipboard"
)

// copyToClipboard copies text to the system clipboard.
// Returns whether it succeeded (silently fails if no clipboard is available).
func copyToClipboard(text string) bool {
	return clipboard.WriteAll(text) == nil
}
