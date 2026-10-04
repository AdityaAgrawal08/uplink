package main

import (
	"fmt"

	"github.com/gen2brain/beeep"
)

func notifyTransferComplete(filename string) {
	_ = beeep.Notify("Uplink-Delta", fmt.Sprintf("Transfer complete: %s", filename), "")
}

func notifyTransferFailed(filename string, err error) {
	_ = beeep.Notify("Uplink-Delta", fmt.Sprintf("Transfer failed: %s - %v", filename, err), "")
}

// notifyMentioned is the desktop ping for an inbound @mention (general room
// only): title + body carry the "mentioned in the chat" message with the
// sender and a message excerpt for context. Fire-and-forget exactly like the
// transfer notices — a headless or no-display environment must never block
// or fail the chat loop (beeep errors are ignored).
func notifyMentioned(sender, excerpt string) {
	_ = beeep.Notify("Uplink-Delta", fmt.Sprintf("%s mentioned you in the chat: %s", sender, excerpt), "")
}

// mentionNotifier is the mention-ping sink. The indirection keeps the chat
// loop testable (tests swap it for a recorder); production always routes
// through notifyMentioned.
var mentionNotifier = notifyMentioned
