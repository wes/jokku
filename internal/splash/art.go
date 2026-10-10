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

// palm is a palm tree on an island against a setting sun, in pixels: each
// letter is a color from palette and a dot is empty. Two rows of pixels
// make one line of text (see halfBlocks), so it stands as tall as the logo
// plus a line above and below.
var palm = []string{
	"............................",
	"...gGGGg.....gGGGg111.......",
	".gGGgggGGg..GGgggGGg111.....",
	"gGg.....gGGGGg1111gGg1111...",
	"G....gGGGGkckGGGg111gG1111..",
	"....gGg..kcTtk1gGg1111G111..",
	"...gG.....Tt22222Gg22222222.",
	"...g......tT222222g22222222.",
	"..........Tt222222222222222.",
	"..........tT333333333333333.",
	".........Tt.................",
	".........tT.333333333333333.",
	"........Tt..................",
	"........tT...4444444444444..",
	".....sSSSSSSs...............",
	"wwwwsSSSSSSSSSswwW3w4wW3wwww",
}

const palmWidth = 28

var palette = map[byte]int{
	'G': 76, 'g': 28, // fronds
	'T': 137, 't': 94, // trunk
	'c': 130, 'k': 58, // coconuts
	'1': 221, '2': yellowOrange, '3': orange, '4': redOrange, // the sun
	's': 180, 'S': 223, // sand
	'w': 31, 'W': 38, // sea
}

// halfBlocks draws pixel rows as text, two pixels to a character: the
// upper half block in the top pixel's color over the bottom one's. It needs
// color; without it the picture is a blob.
func halfBlocks(pixels []string) []string {
	var lines []string
	for y := 0; y < len(pixels); y += 2 {
		top, bottom := pixels[y], strings.Repeat(".", len(pixels[y]))
		if y+1 < len(pixels) {
			bottom = pixels[y+1]
		}
		var b strings.Builder
		for x := 0; x < len(top); x++ {
			t, u := top[x], bottom[x]
			switch {
			case t == '.' && u == '.':
				b.WriteByte(' ')
			case t == '.':
				b.WriteString(fg(palette[u]) + "▄" + reset)
			case u == '.':
				b.WriteString(fg(palette[t]) + "▀" + reset)
			case t == u:
				b.WriteString(fg(palette[t]) + "█" + reset)
			default:
				b.WriteString(fg(palette[t]) + bg(palette[u]) + "▀" + reset)
			}
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
