package tarball

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Exclusions matcher
func shouldExclude(name string, isDir bool) bool {
	// Base name of file/directory
	base := filepath.Base(name)

	// Exact matches
	if base == ".git" || base == ".DS_Store" || base == "Thumbs.db" || base == ".aws" {
		return true
	}

	// Secret/Credential exclusions
	if base == ".env" {
		return true
	}
	if strings.HasPrefix(base, ".env.") && base != ".env.example" {
		return true
	}
	if strings.HasSuffix(base, ".pem") {
		return true
	}
	// Match ssh private keys (e.g. id_rsa, id_ed25519)
	if !isDir && strings.HasPrefix(base, "id_") {
		return true
	}

	return false
}

// Pack directory into a tarball
func Pack(srcDir string, writer io.Writer) error {
	// Ensure srcDir is absolute
	absSrcDir, err := filepath.Abs(srcDir)
	if err != nil {
		return err
	}

	gzw := gzip.NewWriter(writer)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	err = filepath.Walk(absSrcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Relativize path for tar header name
		relPath, err := filepath.Rel(absSrcDir, path)
		if err != nil {
			return err
		}

		if relPath == "." {
			return nil // Skip root directory itself
		}

		// Format slashes consistently for platform compatibility
		relPath = filepath.ToSlash(relPath)

		// Exclude check
		if shouldExclude(path, info.IsDir()) {
			if info.IsDir() {
				return filepath.SkipDir // Skip scanning entire excluded directory
			}
			return nil // Skip file
		}

		// Symbolic link check
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // Ignore symbolic links for safety
		}

		// Create header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}

		header.Name = relPath

		// Security: strip UID, GID, timestamps, and usernames
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.ModTime = time.Time{}
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}

		// Preserve permission bits (read/write/execute)
		header.Mode = int64(info.Mode().Perm())

		err = tw.WriteHeader(header)
		if err != nil {
			return err
		}

		// Write content if it's a regular file
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			_, err = io.Copy(tw, file)
			if err != nil {
				return err
			}
		}

		return nil
	})

	return err
}

// Unpack tarball with Tar-Slip protection.
// Prefer UnpackLimit: unbounded extraction lets a malicious archive
// (decompression bomb) fill the receiver's disk.
func Unpack(reader io.Reader, destDir string) error {
	return UnpackLimit(reader, destDir, 2<<30) // 2 GB backstop
}

// UnpackLimit is Unpack with a cap on total unpacked file bytes. Breach
// aborts with an error (partial files may remain; callers dealing with
// untrusted archives should extract to a fresh temp dir).
func UnpackLimit(reader io.Reader, destDir string, maxBytes int64) error {
	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	gzr, err := gzip.NewReader(reader)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	var written int64
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Clean path and calculate resolved path
		targetPath := filepath.Join(absDestDir, header.Name)
		absTargetPath, err := filepath.Abs(targetPath)
		if err != nil {
			return err
		}

		// Tar-Slip Mitigation: Verify targetPath resides inside destDir
		if !strings.HasPrefix(absTargetPath, absDestDir+string(filepath.Separator)) && absTargetPath != absDestDir {
			return fmt.Errorf("security error: blocked tar-slip path traversal attempt: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			// Preserve mode bits for directory creation
			err = os.MkdirAll(absTargetPath, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

		case tar.TypeReg:
			// Ensure parent dir exists
			parentDir := filepath.Dir(absTargetPath)
			err = os.MkdirAll(parentDir, 0755)
			if err != nil {
				return err
			}

			// Open file for writing, preserving mode bits
			file, err := os.OpenFile(absTargetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			n, err := io.Copy(file, tr)
			file.Close() // Close immediately
			if err != nil {
				return err
			}
			written += n
			if written > maxBytes {
				return fmt.Errorf("security error: archive unpacked size exceeds %d bytes (possible decompression bomb)", maxBytes)
			}

		default:
			// Ignore other type flags (symlinks, devices)
		}
	}

	return nil
}
