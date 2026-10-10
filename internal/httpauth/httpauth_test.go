package httpauth

import (
	"strings"
	"testing"
	"time"
)

// TestTOTPVectors checks codes against RFC 6238's SHA-1 test vectors (their
// 8 digits, cut to the 6 authenticator apps show).
func TestTOTPVectors(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for at, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037",
	} {
		got, err := TOTPCode(secret, time.Unix(at, 0))
		if err != nil || got != want {
			t.Errorf("code at %d: %q, %v; want %s", at, got, err, want)
		}
	}
}

func TestCheckTOTP(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1700000000, 0)
	code, _ := TOTPCode(secret, now)
	step, ok := CheckTOTP(secret, code, now)
	if !ok || step != uint64(now.Unix()/30) {
		t.Fatalf("current code refused: %v %d", ok, step)
	}
	prev, _ := TOTPCode(secret, now.Add(-30*time.Second))
	if _, ok := CheckTOTP(secret, prev, now); !ok {
		t.Error("the previous step's code should still work (clock drift)")
	}
	old, _ := TOTPCode(secret, now.Add(-90*time.Second))
	if _, ok := CheckTOTP(secret, old, now); ok {
		t.Error("a code from three steps ago works")
	}
	if _, ok := CheckTOTP(secret, code[:3]+" "+code[3:], now); !ok {
		t.Error("a code typed with a space is refused")
	}
	for _, bad := range []string{"", "12345", "abcdef", "1234567"} {
		if _, ok := CheckTOTP(secret, bad, now); ok {
			t.Errorf("code %q accepted", bad)
		}
	}
	if !strings.HasPrefix(TOTPURI("Jokku", "wes@example.com", secret), "otpauth://totp/Jokku:wes@example.com?") {
		t.Errorf("unexpected URI %s", TOTPURI("Jokku", "wes@example.com", secret))
	}
}

func TestSignedTokens(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	s := Session{Kind: KindUser, Name: "wes", Version: "v1", Expires: 99}
	tok, err := Sign(key, "session", "app.example.com", s)
	if err != nil {
		t.Fatal(err)
	}
	var got Session
	if !Verify(key, "session", "App.Example.com", tok, &got) || got != s {
		t.Fatalf("round trip: %+v", got)
	}
	for name, ok := range map[string]bool{
		"another host":    Verify(key, "session", "other.example.com", tok, &got),
		"another purpose": Verify(key, "handoff", "app.example.com", tok, &got),
		"another key":     Verify([]byte("another key, just as long as it"), "session", "app.example.com", tok, &got),
		"tampered":        Verify(key, "session", "app.example.com", "e30"+tok[3:], &got),
		"no signature":    Verify(key, "session", "app.example.com", strings.Split(tok, ".")[0], &got),
	} {
		if ok {
			t.Errorf("%s: token accepted", name)
		}
	}
}

func TestPasswords(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse") || CheckPassword(h, "Correct horse") || CheckPassword("", "") {
		t.Error("password check is wrong")
	}
	if _, err := HashPassword(strings.Repeat("x", 73)); err == nil {
		t.Error("a password over bcrypt's limit was hashed (and silently cut)")
	}
	if Fingerprint(h) == Fingerprint(h, "totp") {
		t.Error("fingerprints should differ when the parts do")
	}
}
