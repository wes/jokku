package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Key encrypts a cluster's backups. It is shown to the user once and is
// the only way to read them: Jokku keeps a copy, but losing the cluster
// loses that.
type Key struct{ raw [32]byte }

const keyPrefix = "jbk1_"

func NewKey() *Key {
	var k Key
	rand.Read(k.raw[:])
	return &k
}

// ParseKey reads a key as String writes it.
func ParseKey(s string) (*Key, error) {
	s = strings.TrimSpace(s)
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, keyPrefix))
	if !strings.HasPrefix(s, keyPrefix) || err != nil || len(b) != 32 {
		return nil, errors.New("not a Jokku backup key (they look like " + keyPrefix + "...)")
	}
	var k Key
	copy(k.raw[:], b)
	return &k, nil
}

func (k *Key) String() string { return keyPrefix + base64.RawURLEncoding.EncodeToString(k.raw[:]) }

// ID names the key without giving it away, so a bucket can say which key
// its backups need.
func (k *Key) ID() string { return hex.EncodeToString(k.derive("key id")[:8]) }

func (k *Key) derive(purpose string) []byte {
	m := hmac.New(sha256.New, k.raw[:])
	m.Write([]byte("jokku backups v1: " + purpose))
	return m.Sum(nil)
}

// codec names, compresses and (with a key) encrypts what goes into the
// bucket. Blocks are named by their contents: SHA-256 without a key, an
// HMAC with one, so names give nothing away.
type codec struct {
	names []byte      // the HMAC key for names; nil for plain SHA-256
	aead  cipher.AEAD // nil when not encrypting
}

var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	decoder, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(1<<30))
)

const (
	sealedPlain     = 'P' // zstd
	sealedEncrypted = 'E' // AES-256-GCM over zstd: nonce, then ciphertext
)

func newCodec(k *Key) *codec {
	if k == nil {
		return &codec{}
	}
	block, _ := aes.NewCipher(k.derive("data"))
	aead, _ := cipher.NewGCM(block)
	return &codec{names: k.derive("names"), aead: aead}
}

func (c *codec) name(b []byte) string {
	if c.names == nil {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	m := hmac.New(sha256.New, c.names)
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}

// seal compresses plain and, with a key, encrypts it, bound to what (its
// name), so it can't be passed off as anything else.
func (c *codec) seal(plain []byte, what string) []byte {
	z := encoder.EncodeAll(plain, nil)
	if c.aead == nil {
		return append([]byte{sealedPlain}, z...)
	}
	out := make([]byte, 1+c.aead.NonceSize(), 1+c.aead.NonceSize()+len(z)+c.aead.Overhead())
	out[0] = sealedEncrypted
	rand.Read(out[1:])
	return c.aead.Seal(out, out[1:], z, []byte(what))
}

func (c *codec) open(data []byte, what string) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", what)
	}
	z := data[1:]
	switch {
	case data[0] == sealedEncrypted && c.aead == nil:
		return nil, errors.New("these backups are encrypted, but no key was given")
	case data[0] == sealedPlain && c.aead != nil:
		return nil, errors.New("these backups are not encrypted, but a key was given")
	case data[0] == sealedEncrypted:
		n := c.aead.NonceSize()
		if len(z) < n {
			return nil, fmt.Errorf("%s is damaged", what)
		}
		var err error
		if z, err = c.aead.Open(nil, z[:n], z[n:], []byte(what)); err != nil {
			return nil, fmt.Errorf("%s can't be decrypted with this key, or is damaged", what)
		}
	case data[0] != sealedPlain:
		return nil, fmt.Errorf("%s is not something Jokku wrote", what)
	}
	plain, err := decoder.DecodeAll(z, nil)
	if err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", what, err)
	}
	return plain, nil
}
