package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsClient wraps a gorilla/websocket connection for the chat protocol.
// Replaces long-polling with a persistent bidirectional channel when
// the server supports it. Falls back to HTTP long-polling otherwise.
type wsClient struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	closed bool
}

// wsConnect attempts a WebSocket upgrade. Returns nil if the server
// doesn't support WebSocket (HTTP 404 or connection refused).
func (c *chatClient) wsConnect() *wsClient {
	wsURL := c.serverURL
	if len(wsURL) > 0 && wsURL[len(wsURL)-1] == '/' {
		wsURL = wsURL[:len(wsURL)-1]
	}
	wsURL = convertHTTPToWS(wsURL)
	wsURL += fmt.Sprintf("/api/v1/session/%s/ws", c.key)

	header := http.Header{}
	header.Set("X-Uplink-Username", c.me)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		return nil // fallback to long-polling
	}
	return &wsClient{conn: conn}
}

func convertHTTPToWS(url string) string {
	if len(url) > 8 && url[:8] == "https://" {
		return "wss://" + url[8:]
	}
	if len(url) > 7 && url[:7] == "http://" {
		return "ws://" + url[7:]
	}
	return url
}

// readLoop reads messages from the WebSocket and feeds them to the
// chatClient callbacks. Runs until connection closes.
//
// B24 FIX: Distinguish a clean server-initiated close (going away / normal)
// from an abnormal one, and report the latter through onTransientErr so the
// UI can surface "connection lost" instead of silently freezing. The caller
// (TUI) falls back to HTTP polling when this returns.
func (ws *wsClient) readLoop(c *chatClient) {
	defer ws.Close()
	for {
		_, data, err := ws.conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				if c.onTransientErr != nil {
					c.onTransientErr(fmt.Errorf("websocket read failed: %w", err))
				}
			}
			return
		}
		var poll chatPollResponse
		if err := json.Unmarshal(data, &poll); err != nil {
			continue
		}
		for _, m := range poll.Messages {
			if int64(m.Seq) <= c.lastSeq {
				continue
			}
			c.lastSeq = int64(m.Seq)
			if c.onMessage != nil {
				c.onMessage(m)
			}
		}
		c.applyRoster(poll.ActiveUsers)
		if poll.Ended {
			c.leave()
			if c.onEnded != nil {
				reason := "Session has ended"
				c.onEnded(reason)
			}
			return
		}
	}
}

// heartbeat sends a periodic ping to keep the connection alive.
func (ws *wsClient) heartbeat(c *chatClient) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ws.mu.Lock()
		if ws.closed {
			ws.mu.Unlock()
			return
		}
		err := ws.conn.WriteJSON(map[string]any{"type": "heartbeat"})
		ws.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// send transmits a chat message over WebSocket.
//
// B24 FIX: Include the explicit `type:"chat"` discriminator. Previously the
// payload was `{"text":...}` with no type, so a typed server protocol would
// drop it as an unknown frame.
func (ws *wsClient) send(text, to string) error {
	payload := map[string]any{"type": "chat", "text": text}
	if to != "" {
		payload["to"] = to
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.closed {
		return fmt.Errorf("websocket closed")
	}
	return ws.conn.WriteJSON(payload)
}

// Close shuts down the WebSocket connection.
func (ws *wsClient) Close() {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !ws.closed {
		ws.closed = true
		ws.conn.Close()
	}
}
