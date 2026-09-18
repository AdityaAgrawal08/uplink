package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

// chatMessage is one transcript line. Seq is a LOCAL display counter (the
// server keeps no transcript); MsgId is the global E2E message identity
// used for ACKs and dedup.
type chatMessage struct {
	Seq       int    `json:"seq"`
	MsgId     string `json:"msgId,omitempty"`
	Username  string `json:"username"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	To        string `json:"to,omitempty"` // recipient of a 1:1 message
	ConvID    string `json:"convId"`       // "general" or canonical "a|b"
	CreatedAt string `json:"createdAt"`
}

// conversationKey builds the canonical bucket id for a 1:1 thread.
func conversationKey(a, b string) string {
	pair := []string{a, b}
	sort.Strings(pair)
	return strings.Join(pair, "|")
}

const generalConv = "general"

// runChat picks the rendering mode: full-screen TUI by default, plain lines
// when stdout isn't a terminal or UPLINK_CHAT_PLAIN=1 (tests/CI/pipes).
func runChat(serverURL, key, me string, id *identityKey, password string) {
	if os.Getenv("UPLINK_CHAT_PLAIN") == "1" || !term.IsTerminal(int(os.Stdin.Fd())) {
		runChatPlain(serverURL, key, me, id, password)
		return
	}
	runChatTUI(serverURL, key, me, id, password)
}

// runChatPlain is the headless twin of the bubbletea UI: identical protocol
// logic, line-based rendering. Used by tests/CI and non-TTY environments.
// There is no backlog (the server keeps no transcript) and no local history:
// the room is live from the moment you join.
func runChatPlain(serverURL, key, me string, id *identityKey, password string) {
	sig := &signalClient{serverURL: serverURL, key: key, me: me}
	// Consumer-side exactly-once (mirrors the TUI): every received copy is
	// acked so the sender's backstop graduates, but only the first copy
	// prints. Previously plain mode never acked at all, so senders retried
	// every message 3x and only engine-side swallowing hid the duplicates.
	var eng *engine
	seenChat := newSeenSet(5000)
	seenFile := newSeenSet(2000)
	eng = newEngine(me, id, sig, engineCallbacks{
		onChat: func(c engineChat) {
			if !seenChat.seen("msg:" + c.MsgId) {
				ts := time.Now().Format("15:04")
				if c.To == "" || c.To == me {
					fmt.Printf("[%s] %s: %s\n", ts, c.From, c.Text)
				}
			}
			_ = eng.sendAck(c.From, c.MsgId)
		},
		onFile: func(f engineFile) {
			if seenFile.seen("msg:" + f.MsgId) {
				return
			}
			fmt.Printf("* %s shared %s (%s) -> %s\n", f.From, f.Filename, humanSize(f.Size), f.Path)
		},
		onPeerReady: func(user, code string) {
			fmt.Printf("* encrypted channel to %s (safety %s)\n", user, code)
		},
		onPeerLost: func(user string) {
			fmt.Printf("* lost direct line to %s (fallback relay active)\n", user)
		},
		onError: func(err error) {
			fmt.Printf("* %v\n", err)
		},
		onSignalNote: func(n signalNote) {
			// Headless has no media UI: publish announces are ignored.
		},
	})
	eng.joinPassword = password // enables engine self-rejoin after prune

	// Seed the roster synchronously so the welcome line is accurate; the
	// engine's beat loop keeps it fresh from here on.
	if roster, _, err := sig.heartbeat("", nil); err != nil {
		fmt.Printf("* Warning: presence ping failed (%v)\n", err)
	} else {
		names := []string{}
		for _, m := range roster {
			if m.Online {
				names = append(names, m.Username)
			}
		}
		sort.Strings(names)
		fmt.Printf("* Online (%d): %s\n", len(names), strings.Join(names, ", "))
		eng.setRoster(roster)
	}
	eng.start()
	defer func() {
		if err := sig.leaveRoom(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: leave may not have registered (%v)\n", err)
		}
	}() // registered first → runs second (engine already down)
	defer eng.stop() // registered second → runs first

	fmt.Printf("Connected to session %s as '%s'. Type /exit to leave.\n", key, me)

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch strings.ToLower(line) {
		case "/exit", "/quit":
			fmt.Println("* You left the session.")
			return
		case "/help":
			fmt.Println("* Commands: /exit · anything else broadcasts a message")
		default:
			if _, err := eng.sendChat("", line); err != nil {
				fmt.Printf("* Send failed: %v\n", err)
			}
		}
	}
	// stdin closed (EOF) — treat as exit
	fmt.Println("* Disconnected.")
}

// runChatTUI renders the full-screen bubbletea interface. Defined in
// chat_tui.go; the plain implementation above is its fallback.
