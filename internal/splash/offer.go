package splash

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Asker puts a question to whoever runs setup. ok is false when there is
// no one to ask, or no answer came.
type Asker func(question string) (answer string, ok bool)

// Offer is setup's part. On a server that hasn't decided, it asks whether
// to show the splash at login (JOKKU_SPLASH=1 or 0 answers in advance);
// where it's on, it brings the banner script up to date. Without anyone to
// ask, it says how to turn it on and asks again next time.
func Offer(out io.Writer, exe string, ask Asker) error {
	if !Supported() {
		return nil
	}
	st, err := LoadState()
	if err != nil {
		return err
	}
	switch {
	case st.On:
		return Enable(exe)
	case st.Declined:
		return nil
	}
	on, decided := yesNo(os.Getenv("JOKKU_SPLASH"))
	if !decided {
		q := "-----> Show Jokku's splash screen when you log in to this server?\n" +
			"       It has the logo and live figures about this server and the cluster, in place of the system's welcome text.\n"
		if Theirs() {
			q += "       Your own " + MOTDDir + "/" + scriptName + " is kept, and comes back if you turn the splash off.\n"
		}
		var answer string
		if answer, decided = ask(q + "       Turn it on? [Y/n] "); !decided {
			fmt.Fprintln(out, "-----> To show Jokku's splash screen when you log in, run: sudo jokku splash:enable")
			return nil
		}
		if on, decided = yesNo(answer); !decided {
			on = strings.TrimSpace(answer) == ""
		}
	}
	if !on {
		fmt.Fprintln(out, "       No splash screen. Turn it on later with: sudo jokku splash:enable")
		return Decline()
	}
	if err := Enable(exe); err != nil {
		return err
	}
	fmt.Fprintln(out, "-----> The splash screen shows at every login now. See it with: sudo jokku splash")
	return nil
}

func yesNo(s string) (yes, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "y", "yes", "on", "true":
		return true, true
	case "0", "n", "no", "off", "false":
		return false, true
	}
	return false, false
}

// AskTerminal asks on the controlling terminal: install.sh and update.sh
// run setup with the script itself as stdin. It gives up after two minutes,
// so an unattended run with a terminal still finishes.
func AskTerminal(question string) (string, bool) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", false
	}
	defer tty.Close()
	fmt.Fprint(tty, question)
	answer := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(tty).ReadString('\n')
		if err != nil {
			close(answer)
			return
		}
		answer <- line
	}()
	select {
	case line, ok := <-answer:
		return line, ok
	case <-time.After(2 * time.Minute):
		fmt.Fprintln(tty)
		return "", false
	}
}
