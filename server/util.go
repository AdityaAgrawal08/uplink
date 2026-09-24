package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// ─── Helpers ────────────────────────────────────────────────────────────────

// genId generates a random 16-byte hex string (128 bits of entropy).
func genId() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// jsonMarshal encodes v to JSON.
func jsonMarshal(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("json.Marshal: %w", err)
	}
	return data, nil
}

// jsonUnmarshal decodes JSON data into v.
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// readJSON decodes the request body into v, enforcing a 1 MB limit.
func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

var usernameRegex = regexp.MustCompile(`^[a-zA-Z0-9_]{3,20}$`)

func isValidUsername(u string) bool {
	return usernameRegex.MatchString(u)
}

// ─── Configuration ──────────────────────────────────────────────────────────

type Config struct {
	Port            int
	MaxSessionUsers int
	MessageBufSize  int
	SessionTTL      time.Duration
	HeartbeatTimeout time.Duration
	RateLimitPerSec int
	FileChunkSize   int
	MaxFileSize     int64
}

func DefaultConfig() Config {
	return Config{
		Port:            8080,
		MaxSessionUsers: 50,
		MessageBufSize:  100,
		SessionTTL:      30 * time.Minute,
		HeartbeatTimeout: 45 * time.Second,
		RateLimitPerSec: 20,
		FileChunkSize:   65536, // 64 KB
		MaxFileSize:     256 * 1024 * 1024, // 256 MB
	}
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

func loadConfig() Config {
	cfg := DefaultConfig()
	cfg.Port = envInt("PORT", cfg.Port)
	cfg.MaxSessionUsers = envInt("UPLINK_MAX_USERS", cfg.MaxSessionUsers)
	cfg.MessageBufSize = envInt("UPLINK_MSG_BUF", cfg.MessageBufSize)
	cfg.SessionTTL = envDuration("UPLINK_SESSION_TTL", cfg.SessionTTL)
	cfg.HeartbeatTimeout = envDuration("UPLINK_HB_TIMEOUT", cfg.HeartbeatTimeout)
	cfg.RateLimitPerSec = envInt("UPLINK_RATE_LIMIT", cfg.RateLimitPerSec)
	cfg.MaxFileSize = int64(envInt("UPLINK_MAX_FILE_MB", int(cfg.MaxFileSize/(1024*1024)))) * 1024 * 1024
	return cfg
}

// ─── HTTP helpers ───────────────────────────────────────────────────────────

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonResponse(w, status, map[string]string{"error": msg})
}

// sanitizeHeader strips newline characters from a header value to prevent
// header injection.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

// ─── Graceful shutdown ─────────────────────────────────────────────────────

func waitForShutdownSignal() <-chan os.Signal {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	return sigCh
}

func waitForShutdown(server *http.Server) {
	sig := <-waitForShutdownSignal()
	log.Printf("received %s, shutting down...", sig)
	_ = server.Close()
}
