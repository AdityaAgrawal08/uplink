package main

import (
	"fmt"
	"os"

	"github.com/gen2brain/beeep"
)

// notifyDesktop is the OS desktop-notification call. The indirection keeps
// the fallback path testable (tests force a failure and observe the
// fallback); production always routes through beeep.Notify.
var notifyDesktop = beeep.Notify

// notify delivers ONE desktop notice and never blocks or fails the caller:
// the desktop ping is ALWAYS attempted fire-and-forget (the always-ping
// contract — same behaviour as the transfer notices). If the desktop path
// errors — no notification daemon, no session bus, no notify-send/kdialog,
// the usual headless/SSH/minimal-WM setup — the failure used to vanish
// silently behind "_ = beeep.Notify(...)". It now surfaces: a one-line
// stderr warning naming the reason, then a terminal bell (BEL) so the ping
// still registers audibly on a terminal. stderr is the one stream the TUI
// never paints on (bubbletea owns stdout under the alt screen), so the
// warning can never garble a rendered frame.
func notify(title, body string) {
	if err := notifyDesktop(title, body, ""); err != nil {
		fmt.Fprintf(os.Stderr, "uplink: desktop notification unavailable (%v) — ringing terminal bell instead\n", err)
		fmt.Fprint(os.Stderr, "\a")
	}
}

func notifyTransferComplete(filename string) {
	notify("Uplink-Delta", fmt.Sprintf("Transfer complete: %s", filename))
}

func notifyTransferFailed(filename string, err error) {
	notify("Uplink-Delta", fmt.Sprintf("Transfer failed: %s - %v", filename, err))
}

// beeepNotify is the desktop-notification entry point for the reply ping,
// swappable so tests can capture the exact body without popping real
// toasts; production always routes through beeep.Notify.
var beeepNotify = beeep.Notify

// notifyReplyTo pings the quoted author of an inbound reply. Body is
// EXACTLY "<from> replied to you" — the same beeep path (with the fallback
// surface) mentions use elsewhere.
func notifyReplyTo(from string) {
	_ = beeepNotify("Uplink-Delta", fmt.Sprintf("%s replied to you", from), "")
}

// notifyMentioned is the desktop ping for an inbound @mention (general room
// only): title + body carry exactly "<sender> mentioned you in the chat."
// — one tidy line, no message excerpt. The ping is ALWAYS attempted —
// focused room or not; own sends never ping and DMs are excluded upstream
// (handleNewMessage gates) — and the bell/stderr fallback above keeps a dead
// desktop path visible instead of a silent miss.
func notifyMentioned(sender string) {
	notify("Uplink-Delta", fmt.Sprintf("%s mentioned you in the chat.", sender))
}

// mentionNotifier is the mention-ping sink. The indirection keeps the chat
// loop testable (tests swap it for a recorder); production always routes
// through notifyMentioned.
var mentionNotifier = notifyMentioned

// inviteNotifier lives in groups.go (with the invite payload types it
// renders); it is declared here-adjacent by convention — every desktop ping
// sink is swappable the same way.
