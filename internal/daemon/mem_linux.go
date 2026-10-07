package daemon

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func totalMemoryMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			kb, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")))
			return kb / 1024
		}
	}
	return 0
}
