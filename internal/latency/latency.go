// Package latency keeps how long requests took, for percentiles over the
// last hour and day ("jokku top"): as histograms, which are small, add up
// across nodes exactly, and give a percentile to within 5%.
package latency

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Buckets are log-spaced: bucket 0 holds durations up to minMS, bucket i up
// to minMS·growth^i, and the last everything longer (2.4 minutes and up).
const (
	minMS   = 0.1
	growth  = 1.1
	Buckets = 150
)

func bucketOf(ms float64) int {
	if ms <= minMS {
		return 0
	}
	return min(int(math.Ceil(math.Log(ms/minMS)/math.Log(growth))), Buckets-1)
}

// middle is a duration that stands for bucket i: the geometric middle of its
// bounds, within 5% of anything in it.
func middle(i int) float64 {
	if i == 0 {
		return minMS
	}
	return minMS * math.Pow(growth, float64(i)-0.5)
}

// Histogram counts durations by bucket, leaving out empty ones.
type Histogram map[int]uint64

// Add counts n more durations of about ms.
func (h Histogram) Add(ms float64, n uint64) { h[bucketOf(ms)] += n }

// Merge adds o's counts to h's.
func (h Histogram) Merge(o Histogram) {
	for b, n := range o {
		if b >= 0 && b < Buckets {
			h[b] += n
		}
	}
}

func (h Histogram) Count() uint64 {
	var n uint64
	for _, c := range h {
		n += c
	}
	return n
}

// Percentile is the duration p percent of requests took at most (nearest
// rank), or 0 with none.
func (h Histogram) Percentile(p float64) float64 {
	total := h.Count()
	if total == 0 {
		return 0
	}
	rank := uint64(math.Ceil(p / 100 * float64(total)))
	buckets := make([]int, 0, len(h))
	for b := range h {
		buckets = append(buckets, b)
	}
	sort.Ints(buckets)
	var seen uint64
	for _, b := range buckets {
		seen += h[b]
		if seen >= rank {
			return middle(b)
		}
	}
	return middle(buckets[len(buckets)-1])
}

// Slot is the time one set of histograms covers, and Keep how long they
// are kept.
const (
	Slot = 5 * time.Minute
	Keep = 24 * time.Hour
)

// Recorder keeps a node's request durations for the last day, by app and
// five minutes at a time. The zero value is ready to use.
type Recorder struct {
	mu    sync.Mutex
	slots map[int64]map[string]*[Buckets]uint32 // slot -> app -> counts
}

func slotOf(t time.Time) int64 { return t.Unix() / int64(Slot/time.Second) }

// Add counts a request to app (empty for one no app served) that took ms,
// at t. Requests older than Keep are left out.
func (r *Recorder) Add(t time.Time, app string, ms float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := slotOf(t)
	if s <= slotOf(time.Now().Add(-Keep)) {
		return
	}
	if r.slots == nil {
		r.slots = map[int64]map[string]*[Buckets]uint32{}
	}
	apps := r.slots[s]
	if apps == nil {
		apps = map[string]*[Buckets]uint32{}
		r.slots[s] = apps
	}
	counts := apps[app]
	if counts == nil {
		counts = new([Buckets]uint32)
		apps[app] = counts
	}
	counts[bucketOf(ms)]++
}

// Report is a node's request durations by app over the last hour and day.
// The hour is the slots that started within it, so 55 to 60 minutes.
type Report struct {
	Hour map[string]Histogram `json:"hour"`
	Day  map[string]Histogram `json:"day"`
}

// Report is the last hour's and day's durations as of now; it forgets slots
// older than a day.
func (r *Recorder) Report(now time.Time) Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := Report{Hour: map[string]Histogram{}, Day: map[string]Histogram{}}
	hour, day := slotOf(now.Add(-time.Hour)), slotOf(now.Add(-Keep))
	for s, apps := range r.slots {
		if s <= day {
			delete(r.slots, s)
			continue
		}
		for app, counts := range apps {
			add(rep.Day, app, counts)
			if s > hour {
				add(rep.Hour, app, counts)
			}
		}
	}
	return rep
}

func add(into map[string]Histogram, app string, counts *[Buckets]uint32) {
	h := into[app]
	if h == nil {
		h = Histogram{}
		into[app] = h
	}
	for b, n := range counts {
		if n > 0 {
			h[b] += uint64(n)
		}
	}
}
