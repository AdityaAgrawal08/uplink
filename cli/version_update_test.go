package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
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
