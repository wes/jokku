package props

import (
	"testing"
	"time"
)

func TestSleepAfter(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"15m":    15 * time.Minute,
		"1h30m":  90 * time.Minute,
		"30s":    30 * time.Second,
		"29s":    0, // too short
		"15":     0, // no unit
		"never":  0,
		"":       0,
		"-5m":    0,
		"1h0m0s": time.Hour,
	} {
		if got := SleepAfter(v); got != want {
			t.Errorf("SleepAfter(%q) = %s, want %s", v, got, want)
		}
		if err := Check("ps", "sleep-after", v); (err == nil) != (want > 0 || v == "") {
			t.Errorf("Check(sleep-after %q) = %v", v, err)
		}
	}
}
