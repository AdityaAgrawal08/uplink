package main

import (
	"fmt"

	"github.com/gen2brain/beeep"
)

// beeepNotify is the desktop-notification entry point, swappable so tests
// can capture the exact body without popping real toasts.
var beeepNotify = beeep.Notify

func notifyTransferComplete(filename string) {
	_ = beeepNotify("Uplink-Delta", fmt.Sprintf("Transfer complete: %s", filename), "")
}

func notifyTransferFailed(filename string, err error) {
	_ = beeepNotify("Uplink-Delta", fmt.Sprintf("Transfer failed: %s - %v", filename, err), "")
}

// notifyReplyTo pings the quoted author of an inbound reply. Body is
// EXACTLY "<from> replied to you" — the same beeep path (with the fallback
// surface) mentions use elsewhere.
func notifyReplyTo(from string) {
	_ = beeepNotify("Uplink-Delta", fmt.Sprintf("%s replied to you", from), "")
}
