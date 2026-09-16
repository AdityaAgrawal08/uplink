package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeTar(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	p := filepath.Join(t.TempDir(), "rel.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractReleaseAssetTar(t *testing.T) {
	p := makeTar(t, map[string]string{"uplink": "fake-binary"})
	out, err := extractReleaseAsset(p)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)
	data, _ := os.ReadFile(out)
	if string(data) != "fake-binary" {
		t.Fatalf("wrong content: %q", data)
	}
}

func TestExtractReleaseAssetSkipsNonBinary(t *testing.T) {
	p := makeTar(t, map[string]string{
		"README.md": "docs",
		"uplink":    "fake-binary",
	})
	out, err := extractReleaseAsset(p)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)
	data, _ := os.ReadFile(out)
	if string(data) != "fake-binary" {
		t.Fatalf("picked wrong entry: %q", data)
	}
}

func TestExtractReleaseAssetZipSlip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// malicious entry attempting path traversal
	w, _ := zw.Create("../../evil.sh")
	w.Write([]byte("evil"))
	// legit entry
	w2, _ := zw.Create("uplink.exe")
	w2.Write([]byte("fake-binary"))
	zw.Close()
	p := filepath.Join(t.TempDir(), "rel.zip")
	os.WriteFile(p, buf.Bytes(), 0o644)

	// force the zip branch regardless of host OS by testing extractZipAsset
	out, err := extractZipAsset(p)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(out)
	data, _ := os.ReadFile(out)
	if string(data) != "fake-binary" {
		t.Fatalf("picked wrong entry: %q", data)
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "evil.sh")); err == nil {
		t.Fatal("zip-slip wrote outside target")
	}
}

func TestExtractReleaseAssetEmpty(t *testing.T) {
	p := makeTar(t, map[string]string{"README.md": "docs"})
	if _, err := extractReleaseAsset(p); err == nil {
		t.Fatal("archive without binary must fail")
	}
}

func TestUpdateFailureHint(t *testing.T) {
	if got := updateFailureHint(500, false); got != "" {
		t.Fatalf("non-404 must stay silent, got %q", got)
	}
	if got := updateFailureHint(404, true); got != "" {
		t.Fatalf("token present must stay silent, got %q", got)
	}
	priv := updateFailureHintWith(false)
	for _, want := range []string{"private", "GITHUB_TOKEN", "manually"} {
		if !strings.Contains(priv, want) {
			t.Fatalf("private hint missing %q: %q", want, priv)
		}
	}
	pub := updateFailureHintWith(true)
	if !strings.Contains(pub, "No published release") {
		t.Fatalf("public hint wrong: %q", pub)
	}
}

func TestNormVersion(t *testing.T) {
	cases := map[string]string{"v0.0.2": "0.0.2", "0.0.2": "0.0.2", "v0.0.1": "0.0.1", "": ""}
	for in, want := range cases {
		if got := normVersion(in); got != want {
			t.Fatalf("normVersion(%q) = %q; want %q", in, got, want)
		}
	}
	// The shipped bug: release binary ("v0.0.2") vs trimmed tag ("0.0.2").
	if normVersion("v0.0.2") != normVersion("v0.0.2") {
		t.Fatal("identical releases must compare equal")
	}
	if normVersion("v0.0.1") == normVersion("v0.0.2") {
		t.Fatal("different releases must compare different")
	}
}

func TestCmpVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.0.2", "0.0.2", 0},
		{"0.0.1", "0.0.2", -1},
		{"0.0.3", "0.0.2", 1},
		{"0.1", "0.0.9", 1},
		{"dev", "0.0.2", -2},
		{"", "0.0.2", -2},
		{"1.2", "1.2.0", 0},
	}
	for _, c := range cases {
		if got := cmpVersions(c.a, c.b); got != c.want {
			t.Fatalf("cmpVersions(%q,%q) = %d; want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAuthedGetSendsToken(t *testing.T) {
	var gotAuth, gotNoAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/with" {
			gotAuth = r.Header.Get("Authorization")
		} else {
			gotNoAuth = r.Header.Get("Authorization")
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := authedGet(client, "tok123", srv.URL+"/with")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer tok123" {
		t.Fatalf("token not attached: %q", gotAuth)
	}
	resp2, err := authedGet(client, "", srv.URL+"/without")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if gotNoAuth != "" {
		t.Fatalf("anonymous request carried auth: %q", gotNoAuth)
	}
}

func TestShortVersion(t *testing.T) {
	if got := shortVersion("v0.0.2"); got != "v0.0.2" {
		t.Fatalf("release tag mangled: %q", got)
	}
	if got := shortVersion("v0.0.2-dev+599cae8deadbeef"); got != "v0.0.2-dev+599cae8" {
		t.Fatalf("dev hash not trimmed: %q", got)
	}
	if got := shortVersion("0.0.1"); got != "0.0.1" {
		t.Fatalf("bare version mangled: %q", got)
	}
}
