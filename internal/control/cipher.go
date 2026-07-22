package control

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// ResultCipher encrypts result bytes. aad is authenticated but not stored in
// the ciphertext, binding a result to its task and query identifiers.
type ResultCipher interface {
	Encrypt(plaintext, aad []byte) (nonce, ciphertext []byte, err error)
	Decrypt(nonce, ciphertext, aad []byte) ([]byte, error)
}

// DefaultResultEncryptionKeyID is used for legacy ciphers that do not expose a
// stable key identifier.
const DefaultResultEncryptionKeyID = "local-aes256-gcm-v1"

// ResultCipherKeyer is implemented by ciphers that can identify the result
// encryption key used for newly encrypted rows.
type ResultCipherKeyer interface {
	KeyID() string
}

type AES256GCM struct {
	aead  cipher.AEAD
	rand  io.Reader
	keyID string
}

func NewAES256GCM(key []byte) (*AES256GCM, error) {
	return NewAES256GCMWithKeyID(DefaultResultEncryptionKeyID, key)
}

func NewAES256GCMWithKeyID(keyID string, key []byte) (*AES256GCM, error) {
	keyID, err := normalizeResultEncryptionKeyID(keyID)
	if err != nil {
		return nil, opErr("new cipher", ErrInvalid, err)
	}
	if len(key) != 32 {
		return nil, opErr("new cipher", ErrInvalid, fmt.Errorf("AES-256 key must be exactly 32 bytes, got %d", len(key)))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, opErr("new cipher", ErrInvalid, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, opErr("new cipher", ErrInvalid, err)
	}
	return &AES256GCM{aead: aead, rand: rand.Reader, keyID: keyID}, nil
}

func (c *AES256GCM) KeyID() string {
	if c == nil || c.keyID == "" {
		return DefaultResultEncryptionKeyID
	}
	return c.keyID
}

// ParseAES256Key accepts 64-character hexadecimal or padded/unpadded base64.
func ParseAES256Key(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, opErr("parse data key", ErrInvalid, fmt.Errorf("empty key"))
	}
	if len(value) == 64 {
		if decoded, err := hex.DecodeString(value); err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	encodings := []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding}
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(value)
		if err == nil && len(decoded) == 32 {
			return decoded, nil
		}
	}
	return nil, opErr("parse data key", ErrInvalid, fmt.Errorf("key must encode exactly 32 bytes"))
}

func (c *AES256GCM) Encrypt(plaintext, aad []byte) ([]byte, []byte, error) {
	if c == nil || c.aead == nil {
		return nil, nil, ErrCipherUnavailable
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(c.rand, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	return nonce, c.aead.Seal(nil, nonce, plaintext, aad), nil
}

func (c *AES256GCM) Decrypt(nonce, ciphertext, aad []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, ErrCipherUnavailable
	}
	if len(nonce) != c.aead.NonceSize() {
		return nil, ErrCiphertextInvalid
	}
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, opErr("decrypt result", ErrCiphertextInvalid, err)
	}
	return plaintext, nil
}
