package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

func newKey(t *testing.T, id uint32) Key {
	t.Helper()
	b := make([]byte, KeySize)
	_, _ = rand.Read(b)
	k, err := ParseKey(id, base64.StdEncoding.EncodeToString(b))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newKeyring(t *testing.T, active Key, previous ...Key) *Keyring {
	t.Helper()
	k, err := NewKeyring(active, previous...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTripAndFormat(t *testing.T) {
	t.Parallel()
	k := newKeyring(t, newKey(t, 7))
	ad := []byte("job-spec:0195f0a4-3b7c-7d1e-9f00-123456789abc")
	for _, plain := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte("secret"), 1000)} {
		ct, err := k.Encrypt(plain, ad)
		if err != nil {
			t.Fatal(err)
		}
		if len(ct) != len(plain)+Overhead {
			t.Errorf("len(ct) = %d, want %d", len(ct), len(plain)+Overhead)
		}
		if ct[0] != Version {
			t.Errorf("version byte = %d", ct[0])
		}
		if id, err := KeyID(ct); err != nil || id != 7 {
			t.Errorf("KeyID = (%d, %v), want 7", id, err)
		}
		if len(plain) >= 16 && bytes.Contains(ct, plain) {
			t.Error("ciphertext contains the plaintext")
		}
		got, err := k.Decrypt(ct, ad)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("Decrypt = (%q, %v)", got, err)
		}
	}

	// Random nonces: the same plaintext encrypts differently.
	a, _ := k.Encrypt([]byte("same"), ad)
	b, _ := k.Encrypt([]byte("same"), ad)
	if bytes.Equal(a, b) {
		t.Error("two encryptions are identical")
	}
}

func TestDecryptFailures(t *testing.T) {
	t.Parallel()
	k := newKeyring(t, newKey(t, 1))
	ad := []byte("worker-secret:a")
	ct, err := k.Encrypt([]byte("hunter2"), ad)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte {
		c := bytes.Clone(ct)
		c[i] ^= 0x01
		return c
	}
	unknownKey := bytes.Clone(ct)
	unknownKey[4] = 99

	tests := []struct {
		name string
		ct   []byte
		ad   []byte
		want error
	}{
		{"wrong associated data (swapped record)", ct, []byte("worker-secret:b"), ErrDecrypt},
		{"missing associated data", ct, nil, ErrDecrypt},
		{"tampered nonce", flip(headerSize), ad, ErrDecrypt},
		{"tampered body", flip(headerSize + nonceSize), ad, ErrDecrypt},
		{"tampered tag", flip(len(ct) - 1), ad, ErrDecrypt},
		{"unknown key id", unknownKey, ad, ErrUnknownKey},
		{"unknown version", flip(0), ad, ErrMalformed},
		{"truncated", ct[:Overhead-1], ad, ErrMalformed},
		{"empty", nil, ad, ErrMalformed},
	}
	for _, tt := range tests {
		got, err := k.Decrypt(tt.ct, tt.ad)
		if !errors.Is(err, tt.want) || got != nil {
			t.Errorf("%s: Decrypt = (%q, %v), want %v", tt.name, got, err, tt.want)
		}
	}
}

func TestHeaderIsAuthenticated(t *testing.T) {
	t.Parallel()
	// Relabeling a ciphertext with another known key ID must not decrypt,
	// even if both keys are present.
	k1, k2 := newKey(t, 1), newKey(t, 2)
	k2.Bytes = k1.Bytes // same key material, different ID
	k := newKeyring(t, k1, k2)
	ct, _ := k.Encrypt([]byte("x"), nil)
	ct[4] = 2
	if _, err := k.Decrypt(ct, nil); !errors.Is(err, ErrDecrypt) {
		t.Errorf("relabelled ciphertext: err = %v, want ErrDecrypt", err)
	}
}

func TestRotation(t *testing.T) {
	t.Parallel()
	oldKey, newKeyV := newKey(t, 1), newKey(t, 2)
	ad := []byte("job-spec:j")

	before := newKeyring(t, oldKey)
	oldCT, _ := before.Encrypt([]byte("spec"), ad)

	// New active key, old key kept for decryption.
	after := newKeyring(t, newKeyV, oldKey)
	if got, err := after.Decrypt(oldCT, ad); err != nil || string(got) != "spec" {
		t.Fatalf("decrypt old with rotated keyring = (%q, %v)", got, err)
	}
	newCT, err := after.Encrypt([]byte("spec2"), ad)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := KeyID(newCT); id != 2 {
		t.Errorf("new ciphertext key id = %d, want 2", id)
	}

	rotated, changed, err := after.Rotate(oldCT, ad)
	if err != nil || !changed {
		t.Fatalf("Rotate(old) = (changed %v, %v)", changed, err)
	}
	if id, _ := KeyID(rotated); id != 2 {
		t.Errorf("rotated key id = %d, want 2", id)
	}
	if same, changed, err := after.Rotate(rotated, ad); err != nil || changed || !bytes.Equal(same, rotated) {
		t.Errorf("Rotate(current) = (changed %v, %v), want unchanged", changed, err)
	}
	if _, _, err := after.Rotate(oldCT, []byte("other")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Rotate with wrong AD: err = %v", err)
	}

	// Once the old key is dropped, old ciphertexts are reported clearly.
	dropped := newKeyring(t, newKeyV)
	if _, err := dropped.Decrypt(oldCT, ad); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("decrypt with dropped key: err = %v, want ErrUnknownKey", err)
	}
	if got, err := dropped.Decrypt(rotated, ad); err != nil || string(got) != "spec" {
		t.Errorf("decrypt rotated = (%q, %v)", got, err)
	}
}

func TestParseKey(t *testing.T) {
	t.Parallel()
	raw := bytes.Repeat([]byte{0xfb}, KeySize) // exercises +/ vs -_ alphabets
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		k, err := ParseKey(3, " "+enc.EncodeToString(raw)+"\n")
		if err != nil || !bytes.Equal(k.Bytes, raw) || k.ID != 3 {
			t.Errorf("ParseKey(%v) = (%v, %v)", enc, k, err)
		}
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString(make([]byte, 16)), base64.StdEncoding.EncodeToString(make([]byte, 33))} {
		if _, err := ParseKey(1, bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ParseKey(%q) err = %v, want ErrInvalidKey", bad, err)
		}
	}

	k, err := ParseKeySpec("42:" + base64.StdEncoding.EncodeToString(raw))
	if err != nil || k.ID != 42 {
		t.Errorf("ParseKeySpec = (%v, %v)", k, err)
	}
	for _, bad := range []string{"nokey", "x:" + base64.StdEncoding.EncodeToString(raw), "-1:abc", "4294967296:" + base64.StdEncoding.EncodeToString(raw)} {
		if _, err := ParseKeySpec(bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ParseKeySpec(%q) err = %v, want ErrInvalidKey", bad, err)
		}
	}
}

func TestNewKeyringRejects(t *testing.T) {
	t.Parallel()
	a := newKey(t, 1)
	if _, err := NewKeyring(a, newKey(t, 1)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("duplicate id: err = %v", err)
	}
	if _, err := NewKeyring(Key{ID: 1, Bytes: make([]byte, 16)}); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("short key: err = %v", err)
	}
}
