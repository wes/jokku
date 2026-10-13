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
// each letter is a color from palette and a dot is empty. Its outline is a
// circle 16 pixels across, so the dome widens into the orange band and the
// stripes follow it down. A pixel is one
// character wide and half a line tall (see halfBlocks), about square, so it
// stands as tall as the logo plus a line above and below, and its shades
// change on the same lines as the logo's. Half blocks are in every font
// that draws the logo; quarter blocks, which drew it before, aren't, and
// terminals that borrowed them from another font put them out of line.
var sun = []string{
	"...........yyyyyy...........",
	".........yyyyyyyyyy.........",
	"........yyyyyyyyyyyy........",
	".......yyyyyyyyyyyyyy.......",
	".......yyyyyyyyyyyyyy.......",
	"......yyyyyyyyyyyyyyyy......",
	"......oooooooooooooooo......",
	"......oooooooooooooooo......",
	"......oooooooooooooooo......",
	"............................",
	"......rrrrrrrrrrrrrrrr......",
	"............................",
	".......rrrrrrrrrrrrrr.......",
	"............................",
	".........rrrrrrrrrr.........",
	"............................",
}

const sunWidth = 28

var palette = map[byte]int{'y': yellowOrange, 'o': orange, 'r': redOrange}

// halfBlocks draws pixel rows as text, each pair of rows as one line: a
// full block where both pixels are one color, an upper or lower half block
// where one is empty, and an upper half block over a background color where
// they differ. It needs color; without it the picture is a blob.
func halfBlocks(pixels []string) []string {
	var lines []string
	for y := 0; y+1 < len(pixels); y += 2 {
		var b strings.Builder
		for x := 0; x < len(pixels[y]) && x < len(pixels[y+1]); x++ {
			top, bottom := pixels[y][x], pixels[y+1][x]
			switch {
			case top == '.' && bottom == '.':
				b.WriteByte(' ')
			case top == bottom:
				b.WriteString(fg(palette[top]) + "█" + reset)
			case bottom == '.':
				b.WriteString(fg(palette[top]) + "▀" + reset)
			case top == '.':
				b.WriteString(fg(palette[bottom]) + "▄" + reset)
			default:
				b.WriteString(fg(palette[top]) + bg(palette[bottom]) + "▀" + reset)
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
