// Package httpauth is the cryptography behind logins in front of apps
// (http-auth): password hashes, TOTP codes, and signed session cookies and
// login hand-offs. It keeps no state: a session is valid wherever the
// cluster's session key is, so any ingress node (an edge included) checks
// it on its own.
package httpauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Passwords

// MaxPassword is bcrypt's limit.
const MaxPassword = 72

func HashPassword(password string) (string, error) {
	if len(password) > MaxPassword {
		return "", fmt.Errorf("passwords are at most %d bytes", MaxPassword)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(h), err
}

func CheckPassword(hash, password string) bool {
	return hash != "" && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// TOTP (RFC 6238): 6 digits, 30-second steps, SHA-1, as every authenticator
// app expects.

const totpStep = 30

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret makes a 160-bit secret, base32.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return b32.EncodeToString(b)
}

// TOTPURI is what authenticator apps scan.
func TOTPURI(issuer, user, secret string) string {
	label := url.PathEscape(issuer + ":" + user)
	q := url.Values{"secret": {secret}, "issuer": {issuer}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// TOTPCode is the code for the step at t.
func TOTPCode(secret string, t time.Time) (string, error) {
	return totpCode(secret, uint64(t.Unix()/totpStep))
}

func totpCode(secret string, counter uint64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimRight(secret, "=")))
	if err != nil {
		return "", errors.New("invalid TOTP secret")
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1000000), nil
}

// CheckTOTP accepts the code for the step at now, or the one before or
// after (clocks drift), and returns the step it matched, so a caller can
// refuse a code used twice.
func CheckTOTP(secret, code string, now time.Time) (uint64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return 0, false
	}
	step := uint64(now.Unix() / totpStep)
	for _, c := range []uint64{step, step - 1, step + 1} {
		want, err := totpCode(secret, c)
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return c, true
		}
	}
	return 0, false
}

// Signed tokens: a session cookie, or a hand-off from the login domain to an
// app's domain. Each is bound to a purpose and a host, so one can't be
// replayed as the other, or on another domain.

// Session is who a cookie lets in, where, and until when.
type Session struct {
	Kind    string `json:"k"`           // KindUser, KindPassword or KindShare
	Name    string `json:"n,omitempty"` // the user, or the share's ID
	App     string `json:"a,omitempty"` // the app it's for (password and share sessions)
	Version string `json:"v,omitempty"` // Fingerprint of the credential it was made with
	Expires int64  `json:"e"`
}

const (
	KindUser     = "u"
	KindPassword = "p"
	KindShare    = "s"
)

// Fingerprint identifies a credential (a password hash, a TOTP secret)
// without revealing it: sessions carry it, so changing the credential ends
// them.
func Fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// Sign makes a token for purpose and host carrying v.
func Sign(key []byte, purpose, host string, v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + base64.RawURLEncoding.EncodeToString(mac(key, purpose, host, p)), nil
}

// Verify checks a token made by Sign for the same purpose and host and
// reads it into v.
func Verify(key []byte, purpose, host, token string, v any) bool {
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(key, purpose, host, p)) {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	return err == nil && json.Unmarshal(payload, v) == nil
}

func mac(key []byte, purpose, host, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("jokku-auth\x00" + purpose + "\x00" + strings.ToLower(host) + "\x00" + payload))
	return m.Sum(nil)
}

// Key decodes the cluster's session key.
func Key(s string) ([]byte, error) {
	k, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil || len(k) < 16 {
		return nil, errors.New("invalid session key")
	}
	return k, nil
}

// Token is a random secret for a share link.
func Token() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is how share tokens are stored and compared.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
