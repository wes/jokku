package agent

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/wes/jokku/internal/types"
)

// hostSampler measures the machine from /proc; CPU usage is the change
// since the previous sample.
type hostSampler struct {
	mu                 sync.Mutex
	prevIdle, prevBusy uint64
}

func (h *hostSampler) sample(dataDir string) types.NodeMetrics {
	m := types.NodeMetrics{CPUs: runtime.NumCPU()}
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		line, _, _ := strings.Cut(string(b), "\n")
		f := strings.Fields(line)
		var idle, busy uint64
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			if i == 3 || i == 4 { // idle, iowait
				idle += n
			} else if i < 8 { // user nice system irq softirq steal
				busy += n
			}
		}
		h.mu.Lock()
		if total := (idle - h.prevIdle) + (busy - h.prevBusy); h.prevIdle+h.prevBusy > 0 && total > 0 {
			m.CPUPercent = float64(busy-h.prevBusy) / float64(total) * 100
		}
		h.prevIdle, h.prevBusy = idle, busy
		h.mu.Unlock()
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail int
		for _, line := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(line, ":")
			kb, _ := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")))
			switch k {
			case "MemTotal":
				total = kb / 1024
			case "MemAvailable":
				avail = kb / 1024
			}
		}
		m.MemoryMB, m.MemoryUsedMB = total, total-avail
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			m.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	m.DiskMB, m.DiskFreeMB = diskMB(dataDir)
	return m
}
