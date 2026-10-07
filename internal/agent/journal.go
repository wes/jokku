package agent

import (
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

type journalEntry struct {
	Message   json.RawMessage `json:"MESSAGE"`
	Process   string          `json:"JOKKU_PROCESS"`
	Timestamp string          `json:"__REALTIME_TIMESTAMP"`
}

// format renders "2026-10-07T20:00:00.123456Z app[web.1]: message", the
// format "dokku logs" uses.
func (e journalEntry) format() string {
	us, _ := strconv.ParseInt(e.Timestamp, 10, 64)
	ts := time.UnixMicro(us).UTC().Format("2006-01-02T15:04:05.000000Z")
	return ts + " app[" + e.Process + "]: " + strings.TrimRight(e.message(), "\r\n")
}

// message decodes MESSAGE, which journald emits as a string, or as an array of
// bytes when it is not valid UTF-8.
func (e journalEntry) message() string {
	var s string
	if json.Unmarshal(e.Message, &s) == nil {
		return s
	}
	var b []byte
	if json.Unmarshal(e.Message, &b) == nil {
		return string(b)
	}
	var ints []int
	if json.Unmarshal(e.Message, &ints) == nil {
		buf := make([]byte, len(ints))
		for i, n := range ints {
			buf[i] = byte(n)
		}
		return string(buf)
	}
	return ""
}

func forEachEntry(r io.Reader, fn func(journalEntry)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e journalEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			fn(e)
		}
	}
	return sc.Err()
}

func sortStrings(s []string) { sort.Strings(s) }
