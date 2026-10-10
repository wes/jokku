package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Colors that fade and glow: the traffic view's dots, trails and sparklines
// are drawn cell by cell in colors mixed on the fly, rather than from a few
// fixed styles.

type rgb struct{ r, g, b float64 }

func hex(s string) rgb {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	return rgb{float64(v >> 16 & 255), float64(v >> 8 & 255), float64(v & 255)}
}

// mix is c moved a fraction t of the way to o.
func (c rgb) mix(o rgb, t float64) rgb {
	t = clamp01(t)
	return rgb{c.r + (o.r-c.r)*t, c.g + (o.g-c.g)*t, c.b + (o.b-c.b)*t}
}

func (c rgb) hex() string {
	return fmt.Sprintf("#%02x%02x%02x", uint8(c.r+.5), uint8(c.g+.5), uint8(c.b+.5))
}

func clamp01(t float64) float64 { return math.Max(0, math.Min(1, t)) }

// palette is the TUI's colors as RGB, for the terminal's background, plus
// the few only the animations use.
type palette struct {
	fg, flash, dim, accent, good, warn, bad, cyan rgb

	track rgb     // an idle lane
	glow  rgb     // a busy one
	core  float64 // how much brighter a dot is than its tail
}

func colors() *palette {
	dark := lipgloss.HasDarkBackground()
	pick := func(c lipgloss.AdaptiveColor) rgb {
		if dark {
			return hex(c.Dark)
		}
		return hex(c.Light)
	}
	p := &palette{fg: pick(fg), dim: pick(dim), accent: pick(accent), good: pick(good), warn: pick(warn),
		bad: pick(bad), cyan: pick(cyan)}
	if dark {
		p.flash, p.track, p.glow, p.core = hex("#fff6e9"), hex("#4a3d31"), hex("#b9793a"), .35
	} else {
		p.flash, p.track, p.glow = hex("#8c470a"), hex("#d1c2ab"), hex("#c98a4a")
	}
	return p
}

// painter writes runes in colors, sending a color only when it changes and
// only as well as the terminal can show it.
type painter struct {
	b   strings.Builder
	cur string
}

var sequences = map[rgb]string{}

func (p *painter) put(r rune, c rgb) {
	c = rgb{math.Round(c.r), math.Round(c.g), math.Round(c.b)}
	seq, ok := sequences[c]
	if !ok {
		if len(sequences) > 4096 {
			clear(sequences)
		}
		if s := lipgloss.ColorProfile().Color(c.hex()).Sequence(false); s != "" {
			seq = termenv.CSI + s + "m"
		}
		sequences[c] = seq
	}
	if seq != p.cur {
		p.b.WriteString(seq)
		p.cur = seq
	}
	p.b.WriteRune(r)
}

func (p *painter) String() string {
	if p.cur != "" {
		p.b.WriteString(termenv.CSI + termenv.ResetSeq + "m")
		p.cur = ""
	}
	return p.b.String()
}

var bars = []rune("▁▂▃▄▅▆▇█")

// spark draws vs as bars, from a little under the lowest to the highest so
// that changes show; each column is tinted toward hotColor by as much as
// hot says (0 to 1).
func spark(vs, hot []float64, low, high, hotColor, base rgb) string {
	lo, hi := math.Inf(1), 0.0
	for _, v := range vs {
		if v > 0 {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	lo = math.Max(0, lo-(hi-lo))
	var p painter
	for i, v := range vs {
		if v <= 0 {
			p.put(bars[0], base)
			continue
		}
		f := .5
		if hi > lo {
			f = (v - lo) / (hi - lo)
		}
		level := min(max(int(math.Ceil(f*float64(len(bars)))), 1), len(bars))
		c := low.mix(high, float64(level)/float64(len(bars)))
		if hot != nil {
			c = c.mix(hotColor, hot[i])
		}
		p.put(bars[level-1], c)
	}
	return p.String()
}
