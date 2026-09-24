package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Contract: Go relay implements create/join/ws/leave and explicitly does NOT
// implement the TS/Vercel signaling plane (/heartbeat, /signal, /inbox,
// /inbox/ack, /kick, /admin). This test pins the divergence so silent drift
// in either direction fails loudly. If the relay is revived to replace the
// TS plane, extend routeSession and update this test.
func TestSignalingContractDivergence(t *testing.T) {
	cfg := DefaultConfig()
	srv := NewServer(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/api/v1/session/", srv.routeSession)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Implemented routes must not 404 on method/path shape (auth may 4xx).
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/session/create"},
		{"POST", "/api/v1/session/abc123/join"},
		{"POST", "/api/v1/session/abc123/leave"},
	} {
		req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Fatalf("%s %s should be routed (got 404)", tc.method, tc.path)
		}
	}

	// TS-only routes must 404 on the Go relay (documents the split).
	for _, p := range []string{
		"/api/v1/session/abc/heartbeat",
		"/api/v1/session/abc/signal",
		"/api/v1/session/abc/inbox",
		"/api/v1/session/abc/inbox/ack",
		"/api/v1/session/abc/kick",
		"/api/v1/session/abc/admin",
	} {
		req, err := http.NewRequest("POST", ts.URL+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s should 404 on Go relay (got %d)", p, resp.StatusCode)
		}
	}
}
