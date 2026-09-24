package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const ChunkSizeLimit = 65519 // Chunk size limit to ensure ciphertext (with 16-byte tag) fits in 16-bit length prefix (65519 + 16 = 65535)

func EncryptFileStream(srcPath, dstPath string) (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}

	masterNonce := make([]byte, 12)
	if _, err := rand.Read(masterNonce); err != nil {
		return "", err
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return "", err
	}
	defer dst.Close()

	// Write master nonce to dst header
	if _, err := dst.Write(masterNonce); err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	buf := make([]byte, ChunkSizeLimit)
	var chunkIndex uint64 = 0
	plainHasher := sha256.New()
	var totalPlain int64

	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			plainHasher.Write(buf[:n])
			totalPlain += int64(n)
			nonce := make([]byte, 12)
			copy(nonce, masterNonce)
			binary.BigEndian.PutUint64(nonce[4:], binary.BigEndian.Uint64(nonce[4:])^chunkIndex)

			// Bind chunk index as AAD so reordering/forgery fails.
			var aad [8]byte
			binary.BigEndian.PutUint64(aad[:], chunkIndex)
			ciphertext := aesgcm.Seal(nil, nonce, buf[:n], aad[:])

			lengthBuf := make([]byte, 2)
			binary.BigEndian.PutUint16(lengthBuf, uint16(len(ciphertext)))
			if _, err := dst.Write(lengthBuf); err != nil {
				return "", err
			}

			if _, err := dst.Write(ciphertext); err != nil {
				return "", err
			}
			chunkIndex++
		}

		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return "", err
		}
	}

	// Final trailer frame: AEAD over (chunkCount, totalPlainLen, sha256).
	// Truncation (dropped trailing chunks) now fails verification.
	trailerPlain := make([]byte, 8+8+32)
	binary.BigEndian.PutUint64(trailerPlain[0:8], chunkIndex)
	binary.BigEndian.PutUint64(trailerPlain[8:16], uint64(totalPlain))
	copy(trailerPlain[16:], plainHasher.Sum(nil))
	trailerNonce := make([]byte, 12)
	copy(trailerNonce, masterNonce)
	binary.BigEndian.PutUint64(trailerNonce[4:], binary.BigEndian.Uint64(trailerNonce[4:])^chunkIndex)
	var trailerAAD [8]byte
	binary.BigEndian.PutUint64(trailerAAD[:], chunkIndex)
	trailerCT := aesgcm.Seal(nil, trailerNonce, trailerPlain, trailerAAD[:])
	trailerLen := make([]byte, 2)
	binary.BigEndian.PutUint16(trailerLen, uint16(len(trailerCT)))
	if _, err := dst.Write(trailerLen); err != nil {
		return "", err
	}
	if _, err := dst.Write(trailerCT); err != nil {
		return "", err
	}

	return hex.EncodeToString(key), nil
}

func DecryptFileStream(srcPath, dstPath, keyHex string) error {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return fmt.Errorf("invalid hex key: %w", err)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	masterNonce := make([]byte, 12)
	if _, err := io.ReadFull(src, masterNonce); err != nil {
		return err
	}

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	lengthBuf := make([]byte, 2)
	var chunkIndex uint64 = 0
	plainHasher := sha256.New()
	var totalPlain int64
	var chunks [][]byte

	for {
		_, err := io.ReadFull(src, lengthBuf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		segmentLen := int(binary.BigEndian.Uint16(lengthBuf))
		if segmentLen <= 0 || segmentLen > ChunkSizeLimit+64 {
			return fmt.Errorf("invalid encrypted segment length: %d", segmentLen)
		}

		ciphertext := make([]byte, segmentLen)
		if _, err := io.ReadFull(src, ciphertext); err != nil {
			return fmt.Errorf("truncated archive (missing trailer?): %w", err)
		}

		nonce := make([]byte, 12)
		copy(nonce, masterNonce)
		binary.BigEndian.PutUint64(nonce[4:], binary.BigEndian.Uint64(nonce[4:])^chunkIndex)
		var aad [8]byte
		binary.BigEndian.PutUint64(aad[:], chunkIndex)

		plaintext, err := aesgcm.Open(nil, nonce, ciphertext, aad[:])
		if err != nil {
			return fmt.Errorf("decryption failed at chunk %d (wrong key?): %w", chunkIndex, err)
		}

		// Last frame is the 48-byte trailer (8 count + 8 len + 32 sha).
		// Peek: trailer decrypts to exactly 48 bytes AND a following EOF.
		// We buffer data chunks and only write after trailer verifies, so
		// truncation cannot yield a valid-looking shortened file.
		chunks = append(chunks, plaintext)
		chunkIndex++
	}

	if len(chunks) == 0 {
		return fmt.Errorf("empty encrypted file (missing trailer)")
	}
	trailer := chunks[len(chunks)-1]
	if len(trailer) != 48 {
		return fmt.Errorf("missing integrity trailer (truncated?)")
	}
	dataChunks := chunks[:len(chunks)-1]
	expCount := binary.BigEndian.Uint64(trailer[0:8])
	expLen := binary.BigEndian.Uint64(trailer[8:16])
	expHash := trailer[16:48]
	if expCount != uint64(len(dataChunks)) {
		return fmt.Errorf("chunk count mismatch (truncated?): got %d want %d", len(dataChunks), expCount)
	}
	for _, c := range dataChunks {
		plainHasher.Write(c)
		totalPlain += int64(len(c))
		if _, err := dst.Write(c); err != nil {
			return err
		}
	}
	if uint64(totalPlain) != expLen {
		return fmt.Errorf("length mismatch (truncated?)")
	}
	if subtle.ConstantTimeCompare(plainHasher.Sum(nil), expHash) != 1 {
		return fmt.Errorf("plaintext hash mismatch (tampered?)")
	}
	return nil
}
