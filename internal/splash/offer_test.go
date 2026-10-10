package splash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// asker answers setup's question, and counts how often it was asked.
type asker struct {
	answer string
	ok     bool
	asked  []string
}

func (a *asker) ask(q string) (string, bool) {
	a.asked = append(a.asked, q)
	return a.answer, a.ok
}

func offer(t *testing.T, a *asker) string {
	t.Helper()
	var out strings.Builder
	if err := Offer(&out, "/usr/local/bin/jokku", a.ask); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func on(t *testing.T) bool {
	t.Helper()
	st, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(filepath.Join(MOTDDir, scriptName))
	if st.On != (err == nil) {
		t.Fatalf("state says on=%v, but the script exists=%v", st.On, err == nil)
	}
	return st.On
}

func TestSetupAsksOnceAndPressingEnterSaysYes(t *testing.T) {
	newFakeMOTD(t, true)
	t.Setenv("JOKKU_SPLASH", "")
	a := &asker{answer: "\n", ok: true}
	out := offer(t, a)
	if len(a.asked) != 1 || !strings.HasSuffix(a.asked[0], "Turn it on? [Y/n] ") {
		t.Fatalf("asked %q", a.asked)
	}
	if !on(t) || !strings.Contains(out, "shows at every login now") {
		t.Errorf("not on after yes; said %q", out)
	}
	// Updates keep it, and don't ask again.
	if out := offer(t, a); len(a.asked) != 1 || out != "" || !on(t) {
		t.Errorf("second run asked %d times, said %q", len(a.asked), out)
	}
}

func TestSetupTakesNoForAnAnswer(t *testing.T) {
	newFakeMOTD(t, true)
	t.Setenv("JOKKU_SPLASH", "")
	a := &asker{answer: "n\n", ok: true}
	if out := offer(t, a); on(t) || !strings.Contains(out, "sudo jokku splash:enable") {
		t.Errorf("on after no; said %q", out)
	}
	if offer(t, a); len(a.asked) != 1 || on(t) {
		t.Errorf("asked again after no (%d times)", len(a.asked))
	}
	// splash:enable still works, and a later splash:disable sticks too.
	if err := Enable("/usr/local/bin/jokku"); err != nil || !on(t) {
		t.Fatalf("enable after no: %v", err)
	}
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	if offer(t, a); len(a.asked) != 1 || on(t) {
		t.Errorf("asked again after splash:disable (%d times)", len(a.asked))
	}
}

func TestWithoutATerminalSetupAsksNextTime(t *testing.T) {
	newFakeMOTD(t, true)
	t.Setenv("JOKKU_SPLASH", "")
	out := offer(t, &asker{})
	if on(t) || !strings.Contains(out, "sudo jokku splash:enable") {
		t.Errorf("said %q", out)
	}
	a := &asker{answer: "y\n", ok: true}
	if offer(t, a); len(a.asked) != 1 || !on(t) {
		t.Errorf("the next run didn't ask (%d) or didn't turn it on", len(a.asked))
	}
}

func TestJOKKU_SPLASHAnswersInAdvance(t *testing.T) {
	newFakeMOTD(t, true)
	a := &asker{answer: "y\n", ok: true}
	t.Setenv("JOKKU_SPLASH", "0")
	if offer(t, a); len(a.asked) != 0 || on(t) {
		t.Errorf("JOKKU_SPLASH=0: asked %d times, on %v", len(a.asked), on(t))
	}
	if err := os.RemoveAll(Dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOKKU_SPLASH", "1")
	if offer(t, a); len(a.asked) != 0 || !on(t) {
		t.Errorf("JOKKU_SPLASH=1: asked %d times, on %v", len(a.asked), on(t))
	}
}

func TestSetupMentionsTheAdminsOwnBanner(t *testing.T) {
	f := newFakeMOTD(t, true)
	t.Setenv("JOKKU_SPLASH", "")
	write(t, filepath.Join(f.dir, "00-jokku"), "#!/bin/bash\n")
	a := &asker{answer: "\n", ok: true}
	offer(t, a)
	if !strings.Contains(a.asked[0], "Your own "+f.dir+"/00-jokku is kept") {
		t.Errorf("asked %q", a.asked[0])
	}
}

func TestSetupSkipsServersWithoutUpdateMOTD(t *testing.T) {
	newFakeMOTD(t, true)
	MOTDDir = filepath.Join(t.TempDir(), "missing")
	a := &asker{answer: "\n", ok: true}
	if out := offer(t, a); out != "" || len(a.asked) != 0 {
		t.Errorf("said %q, asked %d times", out, len(a.asked))
	}
}
