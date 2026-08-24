package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

type chatMessage struct {
	Seq       int    `json:"seq"`
	Username  string `json:"username"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

type chatPollResponse struct {
	Messages []chatMessage `json:"messages"`
	Ended    bool          `json:"ended"`
}

type heartbeatResponse struct {
	Ok          bool     `json:"ok"`
	ActiveUsers []string `json:"activeUsers"`
}

type chatClient struct {
	serverURL string
	key       string
	me        string

	lastSeq        int64
	users          []string
	beatFailures   int
	http           *http.Client
	onMessage      func(chatMessage)
	onSystem       func(string)
	onUsers        func([]string)
	onEnded        func(reason string)
	onTransientErr func(error)
}

func newChatClient(serverURL, key, me string) *chatClient {
	return &chatClient{
		serverURL: serverURL,
		key:       key,
		me:        me,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *chatClient) authHeaders() map[string]string {
	return map[string]string{"X-Uplink-Username": c.me}
}

func (c *chatClient) endpoint(path string) string {
	return fmt.Sprintf("%s/api/v1/session/%s%s", c.serverURL, c.key, path)
}

// fetchBacklog seeds history (latest 50) and positions the cursor.
func (c *chatClient) fetchBacklog() ([]chatMessage, error) {
	code, body, err := getJSON(c.endpoint("/messages"), c.authHeaders())
	if err != nil {
		return nil, err
	}
	if code == 403 || code == 410 {
		return nil, fmt.Errorf("session unavailable (%d)", code)
	}
	var poll chatPollResponse
	if err := json.Unmarshal(body, &poll); err != nil {
		return nil, err
	}
	for _, m := range poll.Messages {
		if int64(m.Seq) > c.lastSeq {
			c.lastSeq = int64(m.Seq)
		}
		if c.onMessage != nil {
			c.onMessage(m)
		}
	}
	return poll.Messages, nil
}

// pollOnce fetches messages newer than the cursor and RETURNS them (the TUI
// routes them through Update; plain mode prints via its onMessage hook).
func (c *chatClient) pollOnce() (newMsgs []chatMessage, ended bool, err error) {
	code, body, err := getJSON(c.endpoint(fmt.Sprintf("/messages?after=%d&wait=2500", c.lastSeq)), c.authHeaders())
	if err != nil {
		return nil, false, err
	}
	if code == 410 {
		return nil, true, nil
	}
	if code == 403 {
		return nil, true, fmt.Errorf("kicked from session")
	}
	var poll chatPollResponse
	if err := json.Unmarshal(body, &poll); err != nil {
		return nil, false, err
	}
	for _, m := range poll.Messages {
		if int64(m.Seq) <= c.lastSeq {
			continue
		}
		c.lastSeq = int64(m.Seq)
		newMsgs = append(newMsgs, m)
		if c.onMessage != nil {
			c.onMessage(m)
		}
	}
	return newMsgs, poll.Ended, nil
}

func (c *chatClient) sendMessage(text string) (code int, msg chatMessage, err error) {
	code, body, err := postJSON(c.endpoint("/messages"), map[string]any{"text": text}, c.authHeaders())
	if err != nil {
		return code, msg, err
	}
	var r struct {
		Seq   int    `json:"seq"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &r)
	if r.Seq > 0 {
		msg = chatMessage{Seq: r.Seq, Username: c.me, Kind: "chat", Text: text}
		if int64(r.Seq) > c.lastSeq {
			c.lastSeq = int64(r.Seq) // own message already counted toward cursor
		}
	}
	if r.Error != "" {
		return code, msg, fmt.Errorf("%s", r.Error)
	}
	return code, msg, nil
}

func (c *chatClient) beatOnce() (hb heartbeatResponse, err error) {
	code, body, err := postJSON(c.endpoint("/heartbeat"), map[string]any{}, c.authHeaders())
	if err != nil {
		return hb, err
	}
	if code == 404 {
		return hb, fmt.Errorf("not in session")
	}
	if err := json.Unmarshal(body, &hb); err != nil {
		return hb, err
	}
	if len(hb.ActiveUsers) > 0 {
		c.users = hb.ActiveUsers
		if c.onUsers != nil {
			c.onUsers(c.users)
		}
	}
	return hb, nil
}

func (c *chatClient) leave() {
	_, _, _ = postJSON(c.endpoint("/leave"), map[string]any{}, c.authHeaders())
}

// runChat picks the rendering mode: full-screen TUI by default, plain lines
// when stdout isn't a terminal or UPLINK_CHAT_PLAIN=1 (tests/CI/pipes).
func runChat(serverURL, key, me string) {
	if os.Getenv("UPLINK_CHAT_PLAIN") == "1" || !term.IsTerminal(int(os.Stdin.Fd())) {
		runChatPlain(serverURL, key, me)
		return
	}
	runChatTUI(serverURL, key, me)
}

// runChatPlain is the headless twin of the bubbletea UI: identical protocol
// logic, line-based rendering. Used by tests/CI and non-TTY environments.
func runChatPlain(serverURL, key, me string) {
	client := newChatClient(serverURL, key, me)

	printMsg := func(m chatMessage) {
		ts := time.Now().Format("15:04")
		if m.Kind == "system" {
			// Hide own entry/exit messages from the viewing user
			if m.Text != "" && strings.Contains(m.Text, me) {
				return
			}
			fmt.Printf("[%s] * %s\n", ts, m.Text)
			return
		}
		fmt.Printf("[%s] %s: %s\n", ts, m.Username, m.Text)
	}
	client.onMessage = printMsg
	client.onSystem = func(s string) { fmt.Println("*", s) }
	client.onUsers = func(users []string) {}
	client.onEnded = func(reason string) {
		fmt.Printf("\n* %s — disconnecting.\n", reason)
		os.Exit(0)
	}
	client.onTransientErr = func(err error) {}

	backlog, err := client.fetchBacklog()
	if err != nil {
		fmt.Printf("✗ Failed to load session: %v\n", err)
		os.Exit(1)
	}
	for _, m := range backlog {
		printMsg(m)
	}
	// Register presence immediately (also seeds the roster before the first
	// 15s tick would fire).
	if _, err := client.beatOnce(); err != nil {
		fmt.Printf("* Warning: presence ping failed (%v)\n", err)
	} else {
		fmt.Printf("* Online (%d): %s\n", len(client.users), strings.Join(client.users, ", "))
	}
	fmt.Printf("Connected to session %s as '%s'. Type /exit to leave.\n", key, me)

	go func() {
		beat := time.NewTicker(15 * time.Second)
		defer beat.Stop()
		pollFailures := 0
		for {
			select {
			default:
				newMsgs, ended, err := client.pollOnce()
				if ended {
					reason := "Session has ended"
					if err != nil {
						reason = err.Error()
					}
					client.leave()
					client.onEnded(reason)
					return
				}
				_ = newMsgs // already printed via onMessage hook
				if err != nil {
					pollFailures++
					time.Sleep(time.Duration(400*pollFailures) * time.Millisecond) // backoff
				} else {
					pollFailures = 0
				}
			case <-beat.C:
				if _, err := client.beatOnce(); err != nil {
					client.beatFailures++
					if client.beatFailures >= 3 {
						fmt.Println("\n* Warning: connection to server lost.")
					}
				} else {
					client.beatFailures = 0
				}
			}
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch strings.ToLower(line) {
		case "/exit", "/quit":
			client.leave()
			fmt.Println("* You left the session.")
			os.Exit(0)
		case "/users":
			fmt.Printf("* Online: %s\n", strings.Join(client.users, ", "))
		case "/help":
			fmt.Println("* Commands: /users · /exit · anything else sends a message")
		default:
			code, msg, err := client.sendMessage(line)
			if code == 429 {
				fmt.Println("* Slow down — too many messages.")
			} else if code == 410 {
				client.onEnded("Session has ended")
			} else if err != nil && code != 201 {
				fmt.Printf("* Send failed: %v\n", err)
			} else if code == 201 && client.onMessage != nil {
				client.onMessage(msg) // instant local echo (plain mode)
			}
		}
	}
	// stdin closed (EOF) — treat as exit
	client.leave()
	fmt.Println("* Disconnected.")
}

// runChatTUI renders the full-screen bubbletea interface. Defined in
// chat_tui.go; the plain implementation above is its fallback.
