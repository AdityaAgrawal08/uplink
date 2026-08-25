package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
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

	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			nonce := make([]byte, 12)
			copy(nonce, masterNonce)
			binary.BigEndian.PutUint64(nonce[4:], binary.BigEndian.Uint64(nonce[4:])^chunkIndex)

			ciphertext := aesgcm.Seal(nil, nonce, buf[:n], nil)

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

	for {
		_, err := io.ReadFull(src, lengthBuf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		segmentLen := int(binary.BigEndian.Uint16(lengthBuf))
		if segmentLen <= 0 || segmentLen > ChunkSizeLimit+32 {
			return fmt.Errorf("invalid encrypted segment length: %d", segmentLen)
		}

		ciphertext := make([]byte, segmentLen)
		if _, err := io.ReadFull(src, ciphertext); err != nil {
			return err
		}

		nonce := make([]byte, 12)
		copy(nonce, masterNonce)
		binary.BigEndian.PutUint64(nonce[4:], binary.BigEndian.Uint64(nonce[4:])^chunkIndex)

		plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			return fmt.Errorf("decryption failed at chunk %d (wrong key?): %w", chunkIndex, err)
		}

		if _, err := dst.Write(plaintext); err != nil {
			return err
		}
		chunkIndex++
	}

	return nil
}
