package latency

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

func exact(samples []float64, p float64) float64 {
	s := append([]float64(nil), samples...)
	sort.Float64s(s)
	return s[int(math.Ceil(p/100*float64(len(s))))-1]
}

func TestPercentilesAreWithinFivePercent(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, spread := range []float64{0.5, 1.5, 3} {
		var samples []float64
		h := Histogram{}
		for range 20000 {
			ms := 40 * math.Exp(rng.NormFloat64()*spread) // long-tailed, like real traffic
			samples = append(samples, ms)
			h.Add(ms, 1)
		}
		for _, p := range []float64{50, 95, 99} {
			want, got := exact(samples, p), h.Percentile(p)
			if math.Abs(got-want)/want > 0.05 {
				t.Errorf("spread %v: p%v = %.2f, exactly %.2f", spread, p, got, want)
			}
		}
	}
	if (Histogram{}).Percentile(50) != 0 {
		t.Error("an empty histogram has a percentile")
	}
	h := Histogram{}
	h.Add(0, 1)
	h.Add(1e9, 1)
	if h[0] != 1 || h[Buckets-1] != 1 {
		t.Errorf("the ends don't land in the first and last buckets: %v", h)
	}
}

func TestHistogramsMergeExactly(t *testing.T) {
	a, b, both := Histogram{}, Histogram{}, Histogram{}
	for i := range 1000 {
		ms := float64(i%97) * 3.7
		if i%3 == 0 {
			a.Add(ms, 1)
		} else {
			b.Add(ms, 1)
		}
		both.Add(ms, 1)
	}
	a.Merge(b)
	if a.Count() != 1000 || a.Percentile(95) != both.Percentile(95) || a.Percentile(50) != both.Percentile(50) {
		t.Errorf("merged %d requests, p50 %v p95 %v; want p50 %v p95 %v", a.Count(), a.Percentile(50), a.Percentile(95), both.Percentile(50), both.Percentile(95))
	}
	a.Merge(Histogram{-1: 5, Buckets: 5})
	if a.Count() != 1000 {
		t.Error("buckets out of range were merged")
	}
}

func TestTheRecorderKeepsAnHourAndADay(t *testing.T) {
	now := time.Now()
	var r Recorder
	r.Add(now, "shop", 10)
	r.Add(now.Add(-30*time.Minute), "shop", 20)
	r.Add(now.Add(-2*time.Hour), "shop", 30)
	r.Add(now.Add(-23*time.Hour), "blog", 40)
	r.Add(now.Add(-25*time.Hour), "blog", 50) // too old to keep
	r.Add(now, "", 5)                         // no app served it
	rep := r.Report(now)
	if n := rep.Hour["shop"].Count(); n != 2 {
		t.Errorf("the hour has %d of shop's requests, want 2", n)
	}
	if n := rep.Day["shop"].Count(); n != 3 {
		t.Errorf("the day has %d of shop's requests, want 3", n)
	}
	if _, ok := rep.Hour["blog"]; ok {
		t.Error("blog's request 23 hours ago is in the hour")
	}
	if n := rep.Day["blog"].Count(); n != 1 {
		t.Errorf("the day has %d of blog's requests, want 1 (the one 25 hours ago is gone)", n)
	}
	if rep.Hour[""].Count() != 1 {
		t.Error("requests no app served aren't counted")
	}

	// A day later, it's all forgotten.
	if rep := r.Report(now.Add(Keep + Slot)); len(rep.Day) != 0 || len(r.slots) != 0 {
		t.Errorf("a day later the report has %v and %d slots are kept", rep.Day, len(r.slots))
	}
}
