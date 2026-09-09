package main

import (
	"bufio"
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

const chatUsernameHint = "3-20 chars, letters/digits/underscore"

type sessionCreateResponse struct {
	SessionID string `json:"sessionId"`
}

type sessionJoinResponse struct {
	SessionID    string   `json:"sessionId"`
	Participants []string `json:"participants"`
}

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
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
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
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
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
	persistFlag := fs.Bool("persist", false, "Save chat history to ~/.uplink/history/ on exit")
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

	payload := map[string]any{"username": username, "duration": 600}
	if password != "" {
		payload["password"] = password
	}

	code, body, err := postJSON(serverURL+"/api/v1/session/create", payload, nil)
	if err != nil {
		fmt.Printf("✗ Could not reach server: %v\n", err)
		os.Exit(1)
	}
	if code != 201 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		fmt.Printf("✗ Session creation failed (%d): %s\n", code, e.Error)
		os.Exit(1)
	}

	var created sessionCreateResponse
	if err := json.Unmarshal(body, &created); err != nil || created.SessionID == "" {
		fmt.Println("✗ Unexpected server response.")
		os.Exit(1)
	}

	fmt.Println("\n✓ Session created")
	fmt.Println("\n  ┌─────────────────────────────────────┐")
	fmt.Printf("  │  KEY: %-29s │\n", created.SessionID)
	fmt.Printf("  │  Share it: uplink join %-12s │\n", created.SessionID)
	fmt.Println("  └─────────────────────────────────────┘")
	fmt.Printf("\nYou are '%s'. Connecting to your room… (/exit to leave)\n", username)

	// The creator is already a participant server-side — drop them straight
	// into the room so their username isn't stranded without a UI.
	runChat(serverURL, created.SessionID, username, *persistFlag)
}

// cmdJoinChat handles: uplink join <key> — resolves to the interactive chat.
func cmdJoinChat(args []string, cfg *Config) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	serverFlag := fs.String("server", cfg.Server, "Server base URL")
	passwordFlag := fs.String("password", "", "Session password")
	persistFlag := fs.Bool("persist", false, "Save chat history to ~/.uplink/history/ on exit")
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

	// Unique-username loop — server rejects duplicates with 409.
	var username string
	for {
		username = promptChatUsername(reader)
		payload := map[string]any{"username": username}
		if password != "" {
			payload["password"] = password
		}
		code, body, err := postJSON(serverURL+"/api/v1/session/"+key+"/join", payload, nil)
		if err != nil {
			fmt.Printf("✗ Could not reach server: %v\n", err)
			os.Exit(1)
		}
		if code == 200 {
			break
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		switch code {
		case 403:
			if password == "" {
				fmt.Print("This session is password-protected. Enter password: ")
				pwdBytes, perr := reader.ReadString('\n')
				if perr != nil {
					fmt.Printf("Error reading password: %v\n", perr)
					os.Exit(1)
				}
				password = strings.TrimSpace(pwdBytes)
				continue // retry with password
			}
			fmt.Println("✗ Incorrect password.")
			password = ""
			continue
		case 409:
			fmt.Printf("'%s' is already in this session — choose another.\n", username)
		case 410:
			fmt.Println("✗ This session has ended.")
			os.Exit(1)
		default:
			fmt.Printf("✗ Join failed (%d): %s\n", code, e.Error)
			os.Exit(1)
		}
	}

	runChat(serverURL, key, username, *persistFlag)
}
