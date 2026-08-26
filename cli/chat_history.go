package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type historyEntry struct {
	Seq       int    `json:"seq"`
	Username  string `json:"username"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	To        string `json:"to,omitempty"`
	ConvID    string `json:"convId"`
	CreatedAt string `json:"createdAt"`
	SavedAt   string `json:"savedAt"`
}

type historyFile struct {
	SessionID string         `json:"sessionId"`
	Messages  []historyEntry `json:"messages"`
}

func getHistoryDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".uplink", "history")
}

// saveHistory persists chat messages to ~/.uplink/history/<sessionId>.json.
func saveHistory(sessionID string, msgs []chatMessage) error {
	dir := getHistoryDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	entries := make([]historyEntry, 0, len(msgs))
	for _, m := range msgs {
		entries = append(entries, historyEntry{
			Seq:       m.Seq,
			Username:  m.Username,
			Kind:      m.Kind,
			Text:      m.Text,
			To:        m.To,
			ConvID:    m.ConvID,
			CreatedAt: m.CreatedAt,
			SavedAt:   time.Now().Format(time.RFC3339),
		})
	}

	hf := historyFile{
		SessionID: sessionID,
		Messages:  entries,
	}

	data, err := json.MarshalIndent(hf, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(dir, sessionID+".json")
	return os.WriteFile(path, data, 0o644)
}
