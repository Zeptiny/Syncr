// Package secrets encrypts secrets at rest (job specs, worker secrets) with
// AES-256-GCM. It implements the Cipher ports of internal/app/...:
//
//	Encrypt(plaintext, associatedData []byte) ([]byte, error)
//	Decrypt(ciphertext, associatedData []byte) ([]byte, error)
//
// # Ciphertext format (version 1)
//
//	offset  size  field
//	0       1     version (0x01)
//	1       4     key ID, big-endian uint32
//	5       12    random nonce
//	17      n+16  AES-256-GCM sealed plaintext (n bytes) and tag
//
// The GCM additional data is the 5-byte header followed by the caller's
// associated data, so neither the header nor the binding can be changed
// without failing authentication. Callers bind every ciphertext to its record
// (e.g. "job-spec:<job id>"), so a blob copied into another record does not
// decrypt.
//
// # Keys and rotation
//
// A Keyring holds one active key, used for every Encrypt, and any number of
// previous keys, used only to Decrypt older ciphertexts. To rotate: add a new
// key as active, keep the old one as previous, re-encrypt stored values with
// Rotate (or let them be re-encrypted on their next write), then drop the old
// key. Keys are 32 random bytes, configured as standard base64.
//
// Allowed dependencies: the standard library.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Format constants.
const (
	// Version is the ciphertext format version written by Encrypt.
	Version byte = 1
	// KeySize is the size of an AES-256 key in bytes.
	KeySize = 32

	headerSize = 1 + 4
	nonceSize  = 12
	tagSize    = 16
	// Overhead is the number of bytes Encrypt adds to a plaintext.
	Overhead = headerSize + nonceSize + tagSize
)

// Errors returned (wrapped) by this package.
var (
	// ErrInvalidKey reports a key that is not exactly KeySize bytes of valid
	// base64, or a keyring with duplicate key IDs.
	ErrInvalidKey = errors.New("secrets: invalid key")
	// ErrMalformed reports a ciphertext too short or of an unknown version.
	ErrMalformed = errors.New("secrets: malformed ciphertext")
	// ErrUnknownKey reports a ciphertext sealed with a key ID the keyring
	// does not hold (e.g. a previous key was dropped too early).
	ErrUnknownKey = errors.New("secrets: unknown key id")
	// ErrDecrypt reports a ciphertext that fails authentication: it was
	// tampered with, or the associated data does not match.
	ErrDecrypt = errors.New("secrets: decryption failed")
)

// Key is an AES-256 key and its ID.
type Key struct {
	ID    uint32
	Bytes []byte
}

// ParseKey decodes a base64 key (standard or URL alphabet, padded or not)
// that must be exactly KeySize bytes long.
func ParseKey(id uint32, b64 string) (Key, error) {
	b64 = strings.TrimSpace(b64)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		b, err := enc.DecodeString(b64)
		if err != nil {
			continue
		}
		if len(b) != KeySize {
			return Key{}, fmt.Errorf("%w: key %d is %d bytes, want %d", ErrInvalidKey, id, len(b), KeySize)
		}
		return Key{ID: id, Bytes: b}, nil
	}
	return Key{}, fmt.Errorf("%w: key %d is not valid base64", ErrInvalidKey, id)
}

// ParseKeySpec parses "<id>:<base64>", the form previous keys take in
// configuration.
func ParseKeySpec(spec string) (Key, error) {
	idStr, b64, ok := strings.Cut(spec, ":")
	if !ok {
		return Key{}, fmt.Errorf("%w: want <id>:<base64>", ErrInvalidKey)
	}
	id, err := strconv.ParseUint(strings.TrimSpace(idStr), 10, 32)
	if err != nil {
		return Key{}, fmt.Errorf("%w: key id %q: %w", ErrInvalidKey, idStr, err)
	}
	return ParseKey(uint32(id), b64)
}

// Keyring encrypts with its active key and decrypts with any of its keys.
// It is immutable and safe for concurrent use. Create it with NewKeyring.
type Keyring struct {
	active uint32
	aeads  map[uint32]cipher.AEAD
}

// NewKeyring returns a keyring that encrypts with active and also decrypts
// ciphertexts of the previous keys. Key IDs must be unique.
func NewKeyring(active Key, previous ...Key) (*Keyring, error) {
	k := &Keyring{active: active.ID, aeads: make(map[uint32]cipher.AEAD, 1+len(previous))}
	for _, key := range append([]Key{active}, previous...) {
		if len(key.Bytes) != KeySize {
			return nil, fmt.Errorf("%w: key %d is %d bytes, want %d", ErrInvalidKey, key.ID, len(key.Bytes), KeySize)
		}
		if _, dup := k.aeads[key.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate key id %d", ErrInvalidKey, key.ID)
		}
		block, err := aes.NewCipher(key.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidKey, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidKey, err)
		}
		k.aeads[key.ID] = aead
	}
	return k, nil
}

// ActiveKeyID returns the ID of the key Encrypt uses.
func (k *Keyring) ActiveKeyID() uint32 { return k.active }

// Encrypt seals plaintext with the active key, bound to associatedData.
func (k *Keyring) Encrypt(plaintext, associatedData []byte) ([]byte, error) {
	out := make([]byte, headerSize+nonceSize, headerSize+nonceSize+len(plaintext)+tagSize)
	out[0] = Version
	binary.BigEndian.PutUint32(out[1:headerSize], k.active)
	nonce := out[headerSize : headerSize+nonceSize]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce: %w", err)
	}
	return k.aeads[k.active].Seal(out, nonce, plaintext, additionalData(out[:headerSize], associatedData)), nil
}

// Decrypt opens a ciphertext produced by Encrypt with the same associated
// data, using whichever keyring key sealed it.
func (k *Keyring) Decrypt(ciphertext, associatedData []byte) ([]byte, error) {
	id, err := KeyID(ciphertext)
	if err != nil {
		return nil, err
	}
	aead, ok := k.aeads[id]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownKey, id)
	}
	header := ciphertext[:headerSize]
	nonce := ciphertext[headerSize : headerSize+nonceSize]
	plain, err := aead.Open(nil, nonce, ciphertext[headerSize+nonceSize:], additionalData(header, associatedData))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}

// Rotate re-encrypts ciphertext with the active key if another key sealed
// it. It reports whether it re-encrypted; an unchanged ciphertext is
// returned as is (after verifying it decrypts).
func (k *Keyring) Rotate(ciphertext, associatedData []byte) ([]byte, bool, error) {
	plain, err := k.Decrypt(ciphertext, associatedData)
	if err != nil {
		return nil, false, err
	}
	if id, _ := KeyID(ciphertext); id == k.active {
		return ciphertext, false, nil
	}
	out, err := k.Encrypt(plain, associatedData)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// KeyID returns the ID of the key that sealed ciphertext, without
// decrypting it (e.g. to find values that still need rotation).
func KeyID(ciphertext []byte) (uint32, error) {
	if len(ciphertext) < headerSize+nonceSize+tagSize {
		return 0, fmt.Errorf("%w: %d bytes", ErrMalformed, len(ciphertext))
	}
	if ciphertext[0] != Version {
		return 0, fmt.Errorf("%w: version %d", ErrMalformed, ciphertext[0])
	}
	return binary.BigEndian.Uint32(ciphertext[1:headerSize]), nil
}

func additionalData(header, associatedData []byte) []byte {
	ad := make([]byte, 0, len(header)+len(associatedData))
	return append(append(ad, header...), associatedData...)
}
