package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"
)

var usernameRegex = regexp.MustCompile(`^[a-zA-Z0-9_]{3,20}$`)

// B25 FIX: shared HTTP client for all JSON API calls so keep-alive
// connections are reused across polls, heartbeats, and sends.
var sharedHTTPClient = &http.Client{Timeout: 15 * time.Second}

const chatUsernameHint = "3-20 chars, letters/digits/underscore"

// promptLine asks until non-empty; trims spaces and surrounding quotes.
func promptLine(reader *bufio.Reader, label string) string {
	for {
		fmt.Printf("%s", label)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			fmt.Println()
			os.Exit(1)
		}
		line = strings.Trim(strings.TrimSpace(line), "\"'")
		if line != "" {
			return line
		}
	}
}

func promptChatUsername(reader *bufio.Reader) string {
	for {
		u := promptLine(reader, "Username: ")
		if usernameRegex.MatchString(u) {
			return u
		}
		fmt.Printf("Invalid username (%s). Try again.\n", chatUsernameHint)
	}
}

func postJSON(url string, payload any, headers map[string]string) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// B25 FIX: reuse a shared client so keep-alive connections are pooled.
	// Previously every call allocated a new http.Client (new pool, new
	// dials), defeating HTTP keep-alive on every poll/heartbeat.
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func getJSON(url string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// B25 FIX: shared client (see postJSON).
	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// cmdCreateSession handles: uplink create session
func cmdCreateSession(args []string, cfg *Config) {
	fs := flag.NewFlagSet("create session", flag.ExitOnError)
	serverFlag := fs.String("server", cfg.Server, "Server base URL")
	passwordFlag := fs.String("password", "", "Password-protect this session")
	if err := fs.Parse(normalizeFlagOrder(args, map[string]bool{"server": true, "password": true})); err != nil {
		os.Exit(1)
	}
	serverURL := sanitizeServerUrl(*serverFlag)

	reader := bufio.NewReader(os.Stdin)
	username := promptChatUsername(reader)

	password := *passwordFlag
	if password == "" && term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Print("Session password (optional, press Enter to skip): ")
		pwdBytes, err := reader.ReadString('\n')
		if err == nil {
			password = strings.TrimSpace(pwdBytes)
		}
	}

	// Device identity: the public half is advertised in the roster so peers
	// can E2E-encrypt; rooms carry no duration (they live till empty).
	id, err := loadOrCreateIdentity()
	if err != nil {
		fmt.Printf("✗ Could not load device identity: %v\n", err)
		os.Exit(1)
	}
	sc := &signalClient{serverURL: serverURL, me: username}
	sid, err := sc.createRoom(username, base64.StdEncoding.EncodeToString(id.publicKey()), password)
	if err != nil {
		fmt.Printf("✗ Session creation failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n✓ Session created")
	fmt.Println("\n  ┌─────────────────────────────────────┐")
	fmt.Printf("  │  KEY: %-29s │\n", sid)
	fmt.Printf("  │  Share it: uplink join %-12s │\n", sid)
	fmt.Println("  └─────────────────────────────────────┘")
	fmt.Printf("\nYou are '%s'. Connecting to your room… (Ctrl+C to leave)\n", username)

	// The creator is already a participant server-side — drop them straight
	// into the room so their username isn't stranded without a UI.
	runChat(serverURL, sid, username, id, password)
}

// cmdJoinChat handles: uplink join <key> — resolves to the interactive chat.
func cmdJoinChat(args []string, cfg *Config) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	serverFlag := fs.String("server", cfg.Server, "Server base URL")
	passwordFlag := fs.String("password", "", "Session password")
	if err := fs.Parse(normalizeFlagOrder(args, map[string]bool{"server": true, "password": true})); err != nil {
		os.Exit(1)
	}
	serverURL := sanitizeServerUrl(*serverFlag)

	key := ""
	if fs.NArg() >= 1 {
		key = strings.TrimSpace(fs.Arg(0))
	}
	reader := bufio.NewReader(os.Stdin)
	for key == "" {
		key = promptLine(reader, "Session key (6 digits): ")
		if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(key) {
			fmt.Println("Key must be exactly 6 digits.")
			key = ""
		}
	}

	password := *passwordFlag
	id, err := loadOrCreateIdentity()
	if err != nil {
		fmt.Printf("✗ Could not load device identity: %v\n", err)
		os.Exit(1)
	}
	pubkey := base64.StdEncoding.EncodeToString(id.publicKey())

	// Unique-username loop — server rejects duplicates with 409, missing
	// passwords with 401, and gone rooms with 404 (rooms die on empty).
	// Transients (5xx, network) retry with backoff; anything else exits.
	username := promptChatUsername(reader)
	transientFails := 0
	for {
		sc := &signalClient{serverURL: serverURL, me: username, key: key}
		_, err := sc.joinRoom(username, pubkey, password)
		if err == nil {
			break
		}
		msg := err.Error()
		switch code := apiStatusCode(err); {
		case code == 401:
			fmt.Print("Password required or incorrect. Enter password: ")
			pwdBytes, perr := reader.ReadString('\n')
			if perr != nil {
				fmt.Printf("Error reading password: %v\n", perr)
				os.Exit(1)
			}
			password = strings.TrimSpace(pwdBytes)
			transientFails = 0
			continue
		case code == 409:
			fmt.Printf("'%s' is already in this session — choose another.\n", username)
			username = promptChatUsername(reader)
			transientFails = 0
		case code == 404:
			fmt.Println("✗ Session not found — check the 6-digit code (rooms vanish when emptied).")
			os.Exit(1)
		case code >= 500 || code == 0:
			transientFails++
			if transientFails > 3 {
				fmt.Printf("✗ Join failed: %s\n", msg)
				os.Exit(1)
			}
			time.Sleep(time.Duration(transientFails) * 2 * time.Second)
		default:
			fmt.Printf("✗ Join failed: %s\n", msg)
			os.Exit(1)
		}
	}

	runChat(serverURL, key, username, id, password)
}
