package tarball

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestPackUnpack(t *testing.T) {
	// Create source temp dir
	srcDir, err := os.MkdirTemp("", "tarball_test_src")
	if err != nil {
		t.Fatalf("Failed to create temp src dir: %v", err)
	}
	defer os.RemoveAll(srcDir)

	// Create a sub-file
	file1Name := "test1.txt"
	file1Content := []byte("hello world")
	err = os.WriteFile(filepath.Join(srcDir, file1Name), file1Content, 0644)
	if err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}

	// Pack source dir into buffer
	var buf bytes.Buffer
	err = Pack(srcDir, &buf)
	if err != nil {
		t.Fatalf("Pack failed: %v", err)
	}

	// Create dest temp dir
	destDir, err := os.MkdirTemp("", "tarball_test_dest")
	if err != nil {
		t.Fatalf("Failed to create temp dest dir: %v", err)
	}
	defer os.RemoveAll(destDir)

	// Unpack buffer to dest dir
	err = Unpack(&buf, destDir)
	if err != nil {
		t.Fatalf("Unpack failed: %v", err)
	}

	// Verify file1 content in dest dir
	destFile1 := filepath.Join(destDir, file1Name)
	content, err := os.ReadFile(destFile1)
	if err != nil {
		t.Fatalf("Failed to read unpacked file1: %v", err)
	}

	if !bytes.Equal(content, file1Content) {
		t.Errorf("Expected content %s, got %s", string(file1Content), string(content))
	}
}

func TestTarSlipProtection(t *testing.T) {
	// Create a malicious tarball buffer containing a path traversal filename
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	header := &tar.Header{
		Name: "../malicious_file.txt",
		Mode: 0644,
		Size: 17,
	}
	err := tw.WriteHeader(header)
	if err != nil {
		t.Fatalf("Failed to write tar header: %v", err)
	}
	_, err = tw.Write([]byte("malicious content"))
	if err != nil {
		t.Fatalf("Failed to write tar body: %v", err)
	}
	tw.Close()
	gzw.Close()

	// Create dest temp dir
	destDir, err := os.MkdirTemp("", "tarslip_test_dest")
	if err != nil {
		t.Fatalf("Failed to create temp dest dir: %v", err)
	}
	defer os.RemoveAll(destDir)

	// Attempt unpacking and verify it returns a path traversal warning error
	err = Unpack(&buf, destDir)
	if err == nil {
		t.Error("Expected error from path traversal, but Unpack succeeded")
	} else if !bytes.Contains([]byte(err.Error()), []byte("blocked tar-slip path traversal attempt")) {
		t.Errorf("Expected path traversal warning error, got: %v", err)
	}
}

func TestUnpackLimitStopsBomb(t *testing.T) {
	// 20 MB of zeros compresses to ~20 KB: a classic decompression bomb.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	hdr := &tar.Header{Name: "zeros.bin", Mode: 0o644, Size: 20 << 20}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for written := 0; written < 20; written++ {
		if _, err := tw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gzw.Close()

	destDir, err := os.MkdirTemp("", "bomb_test_dest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(destDir)

	// Same archive under a generous cap extracts fine.
	if err := UnpackLimit(bytes.NewReader(buf.Bytes()), destDir, 64<<20); err != nil {
		t.Fatalf("generous cap should pass: %v", err)
	}

	destDir2, err := os.MkdirTemp("", "bomb_test_dest2")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(destDir2)

	// Under a tight cap it must abort, not fill the disk.
	if err := UnpackLimit(bytes.NewReader(buf.Bytes()), destDir2, 1<<20); err == nil {
		t.Fatal("bomb must be rejected under tight cap")
	}
}
