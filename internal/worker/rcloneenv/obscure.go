package rcloneenv

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// obscureKey is rclone's fixed key for obscuring config values
// (fs/config/obscure). Obscuring is not encryption: it only keeps values from
// being readable at a glance, and anyone can reveal them.
const obscureKey = "\x9c\x93\x5b\x48\x73\x0a\x55\x4d\x6b\xfd\x7c\x63\xc8\x86\xa9\x2b" +
	"\xd3\x90\x19\x8e\xb8\x12\x8a\xfb\xf4\xde\x16\x2b\x8b\x95\xf6\x38"

// Obscure returns plain in the form "rclone obscure" produces and rclone
// expects for options such as sftp's "pass": base64url (no padding) of a
// random 16-byte IV followed by plain encrypted with AES-256-CTR under
// rclone's fixed key.
//
// It reimplements github.com/rclone/rclone/fs/config/obscure, which imports
// rclone's whole fs package (and its dependency tree) and keeps a lazily
// initialized package-level cipher; the algorithm is small and is tested
// against the real binary.
func Obscure(plain string) (string, error) {
	return obscure(plain, rand.Reader)
}

func obscure(plain string, random io.Reader) (string, error) {
	block, err := aes.NewCipher([]byte(obscureKey))
	if err != nil {
		return "", fmt.Errorf("obscure: %w", err)
	}
	out := make([]byte, aes.BlockSize+len(plain))
	iv := out[:aes.BlockSize]
	if _, err := io.ReadFull(random, iv); err != nil {
		return "", fmt.Errorf("obscure: read IV: %w", err)
	}
	cipher.NewCTR(block, iv).XORKeyStream( //nolint:gosec // G407 false positive: iv was just filled from random
		out[aes.BlockSize:], []byte(plain))
	return base64.RawURLEncoding.EncodeToString(out), nil
}
