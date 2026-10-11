package splash

import (
	"strconv"
	"strings"
)

// logo is "JOKKU" in the ANSI Shadow figlet font, two rows to each shade.
var logo = []string{
	"     ██╗ ██████╗ ██╗  ██╗██╗  ██╗██╗   ██╗",
	"     ██║██╔═══██╗██║ ██╔╝██║ ██╔╝██║   ██║",
	"     ██║██║   ██║█████╔╝ █████╔╝ ██║   ██║",
	"██   ██║██║   ██║██╔═██╗ ██╔═██╗ ██║   ██║",
	"╚█████╔╝╚██████╔╝██║  ██╗██║  ██╗╚██████╔╝",
	" ╚════╝  ╚═════╝ ╚═╝  ╚═╝╚═╝  ╚═╝ ╚═════╝ ",
}

const logoWidth = 41

// Shades of the logo, top to bottom, as xterm-256 colors.
const (
	yellowOrange = 214
	orange       = 208
	redOrange    = 202
)

var logoShades = []int{yellowOrange, yellowOrange, orange, orange, redOrange, redOrange}

// sun is a striped sun, the way the eighties drew a sunset, in pixels:
// each letter is a color from palette and a dot is empty. Two pixels
// across and two down make one character (see quarterBlocks), so it stands
// as tall as the logo plus a line above and below, and its shades change
// on the same lines as the logo's.
var sun = []string{
	"......................yyyyyyyyyyyy......................",
	"...................yyyyyyyyyyyyyyyyyy...................",
	"................yyyyyyyyyyyyyyyyyyyyyyyy................",
	"...............yyyyyyyyyyyyyyyyyyyyyyyyyy...............",
	"..............yyyyyyyyyyyyyyyyyyyyyyyyyyyy..............",
	".............yyyyyyyyyyyyyyyyyyyyyyyyyyyyyy.............",
	"............oooooooooooooooooooooooooooooooo............",
	"............oooooooooooooooooooooooooooooooo............",
	"............oooooooooooooooooooooooooooooooo............",
	"........................................................",
	".............rrrrrrrrrrrrrrrrrrrrrrrrrrrrrr.............",
	"........................................................",
	"...............rrrrrrrrrrrrrrrrrrrrrrrrrr...............",
	"........................................................",
	"...................rrrrrrrrrrrrrrrrrr...................",
	"........................................................",
}

const sunWidth = 28

var palette = map[byte]int{'y': yellowOrange, 'o': orange, 'r': redOrange}

// quadrants are the block characters for each set of filled quarters of a
// character: bit 0 is the top left, 1 the top right, 2 the bottom left and
// 3 the bottom right.
var quadrants = []rune(" ▘▝▀▖▌▞▛▗▚▐▜▄▙▟█")

// quarterBlocks draws pixel rows as text, each square of four pixels as
// one character: a quarter block in one color over another, or over the
// terminal's background where pixels are empty. A square holds at most two
// colors, an empty pixel counting as one. It needs color; without it the
// picture is a blob.
func quarterBlocks(pixels []string) []string {
	var lines []string
	for y := 0; y+1 < len(pixels); y += 2 {
		var b strings.Builder
		for x := 0; x+1 < len(pixels[y]); x += 2 {
			square := [4]byte{pixels[y][x], pixels[y][x+1], pixels[y+1][x], pixels[y+1][x+1]}
			ink, paper := byte('.'), byte('.')
			for _, p := range square {
				switch {
				case p == '.':
				case ink == '.':
					ink = p
				case p != ink:
					paper = p
				}
			}
			if ink == '.' {
				b.WriteByte(' ')
				continue
			}
			bits := 0
			for i, p := range square {
				if p == ink {
					bits |= 1 << i
				}
			}
			b.WriteString(fg(palette[ink]))
			if paper != '.' {
				b.WriteString(bg(palette[paper]))
			}
			b.WriteString(string(quadrants[bits]) + reset)
		}
		lines = append(lines, b.String())
	}
	return lines
}

const (
	reset = "\x1b[0m"
	bold  = "\x1b[1m"
	dim   = "\x1b[2m"
)

func fg(c int) string { return "\x1b[38;5;" + strconv.Itoa(c) + "m" }
func bg(c int) string { return "\x1b[48;5;" + strconv.Itoa(c) + "m" }
