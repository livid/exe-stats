package main

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"math"
)

// The Hit Counter, a site's own badge: badge.py's drawing, here in Go
// and fed one site's numbers instead of a made-up day. Its wheels hold
// the page views since the site was added; its bars, today's, two hours
// a bar from midnight where the site's days begin.
//
// The numbers stand still: the wheels and the bars are the site as it
// is. Only the lamp at the top right moves, breathing — it rests a few
// seconds, its glow rises through a dim ring to the full one and falls
// back. The first frame is the lamp at rest, so a reader who stops
// animations has the whole badge.
//
// The face, the glyphs and the palette are badge.py's, in its order, with
// one colour more for the dim ring; the test draws badge.gif's first
// frame here and compares the two.

const cw, ch = 88, 31 // the badge, in px

// the palette, by the letters badge.py names its colours with
var counterInk = []struct {
	c   byte
	rgb color.RGBA
}{
	{'K', color.RGBA{0x26, 0x26, 0x26, 0xff}}, // the Platinum black: the frame and the counter's case
	{'k', color.RGBA{0x00, 0x00, 0x00, 0xff}}, // the wheels
	{'W', color.RGBA{0xff, 0xff, 0xff, 0xff}}, // the face, the name and the numbers
	{'I', color.RGBA{0x33, 0x33, 0x99, 0xff}}, // the highlight indigo: the band, and a bar's top
	{'i', color.RGBA{0x5c, 0x5c, 0xb8, 0xff}}, // the band catching the light
	{'L', color.RGBA{0x99, 0x99, 0xff, 0xff}}, // a bar, in the desk's deeper lavender
	{'O', color.RGBA{0xff, 0x66, 0x00, 0xff}}, // the visitors' orange: the last wheel, the lamp, the bar still filling
	{'o', color.RGBA{0xff, 0xcc, 0x99, 0xff}}, // the bar still filling, as the desk draws it
	{'r', color.RGBA{0xa8, 0x44, 0x00, 0xff}}, // the orange turning away on its wheel
	{'R', color.RGBA{0x55, 0x22, 0x00, 0xff}}, // and further
	{'S', color.RGBA{0x99, 0x99, 0x99, 0xff}}, // the window's sunken edge
	{'d', color.RGBA{0x55, 0x55, 0x55, 0xff}}, // between the wheels
	{'b', color.RGBA{0xbb, 0xbb, 0xbb, 0xff}}, // a number turning away
	{'m', color.RGBA{0x77, 0x77, 0x77, 0xff}}, // and further
	{'g', color.RGBA{0xaa, 0x77, 0x99, 0xff}}, // the lamp's glow rising, halfway from the band to its own
}

var counterPal, counterIndex = func() (color.Palette, [256]uint8) {
	var p color.Palette
	var idx [256]uint8
	for i, k := range counterInk {
		p = append(p, k.rgb)
		idx[k.c] = uint8(i)
	}
	return p, idx
}()

// the name: two-pixel stems on a six-row x-height, the t two rows taller
var counterName = map[rune][]string{
	'e': {".####.", "##..##", "######", "##....", "##...#", ".####."},
	'x': {"##..##", "##..##", ".####.", ".####.", "##..##", "##..##"},
	'-': {"....", "....", "####", "....", "....", "...."},
	's': {".#####", "##....", ".####.", "....##", "....##", "#####."},
	't': {".##..", ".##..", "#####", ".##..", ".##..", ".##..", ".##..", "..###"},
	'a': {".####.", "....##", ".#####", "##..##", "##..##", ".#####"},
}

const nameX, nameBase = 4, 10 // the name's left edge, and the row under its x-height

var (
	lampArt = []string{".###.", "#####", "#####", "#####", ".###."}
	glowArt = []string{"..###..", ".#...#.", "#.....#", "#.....#", "#.....#", ".#...#.", "..###.."}
)

const lampX, lampY = 80, 4 // its top left

// the wheels' numbers, five by seven, and the letters a last wheel
// wears once the count is too long for five
var (
	digitArt = [10][]string{
		{".###.", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."},
		{"..#..", ".##..", "..#..", "..#..", "..#..", "..#..", ".###."},
		{".###.", "#...#", "....#", "...#.", "..#..", ".#...", "#####"},
		{".###.", "#...#", "....#", "..##.", "....#", "#...#", ".###."},
		{"...#.", "..##.", ".#.#.", "#..#.", "#####", "...#.", "...#."},
		{"#####", "#....", "####.", "....#", "....#", "#...#", ".###."},
		{"..##.", ".#...", "#....", "####.", "#...#", "#...#", ".###."},
		{"#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."},
		{".###.", "#...#", "#...#", ".###.", "#...#", "#...#", ".###."},
		{".###.", "#...#", "#...#", ".####", "....#", "...#.", ".##.."},
	}
	kArt = []string{"#...#", "#..#.", "#.#..", "##...", "#.#..", "#..#.", "#...#"}
	mArt = []string{"#...#", "##.##", "#.#.#", "#.#.#", "#...#", "#...#", "#...#"}
)

const (
	wheels   = 5
	caseX    = 4  // the counter's case, 41 by 13, top left
	caseY    = 15 //
	pitch    = 10 // a number and the gap to the next, round the wheel
	barsX    = 49 // the first bar's left edge
	barsBase = 26 // and the bars' bottom row
	barTop   = 12 // the tallest bar, px
)

// face on, turning, turned away: white wheels, and the last one orange
var wheelInk = map[bool][3]byte{false: {'W', 'b', 'm'}, true: {'O', 'r', 'R'}}

type canvas [ch][cw]byte

func counterFace() *canvas {
	var c canvas
	for y := range ch {
		for x := range cw {
			c[y][x] = 'W'
		}
	}
	for x := range cw {
		c[0][x], c[ch-1][x], c[11][x] = 'K', 'K', 'K' // the frame, and the band's foot
	}
	for y := range ch {
		c[y][0], c[y][cw-1] = 'K', 'K'
	}
	for y := 1; y < 11; y++ {
		for x := 1; x < cw-1; x++ {
			c[y][x] = 'I'
			if y == 1 {
				c[y][x] = 'i'
			}
		}
	}
	x := nameX
	for _, r := range "exe-stats" {
		g := counterName[r]
		c.art(g, x, nameBase-len(g), 'W')
		x += len(g[0]) + 1
	}
	// the counter's case, sunk into the face, and the chart's floor
	for x := caseX - 1; x <= caseX+wheels*8; x++ {
		c[caseY-1][x] = 'S'
	}
	for y := caseY - 1; y < caseY+13; y++ {
		c[y][caseX-1] = 'S'
	}
	for y := caseY; y < caseY+13; y++ {
		for x := caseX; x <= caseX+wheels*8; x++ {
			if y == caseY || y == caseY+12 || x == caseX || x == caseX+wheels*8 {
				c[y][x] = 'K'
			} else {
				c[y][x] = 'd'
			}
		}
	}
	for x := barsX - 1; x < barsX+12*3; x++ {
		c[barsBase+1][x] = 'K'
	}
	return &c
}

// art draws a glyph's # pixels in ink, its top left at x, y.
func (c *canvas) art(g []string, x, y int, ink byte) {
	for r, row := range g {
		for j := range len(row) {
			if row[j] == '#' {
				c[y+r][x+j] = ink
			}
		}
	}
}

// lamp is the live lamp, glow 0 at rest; 1 its glint lit and a dim
// ring round it; 2 the ring in its own colour, as badge.py lights it.
func (c *canvas) lamp(glow int) {
	c.art(lampArt, lampX, lampY, 'O')
	c[lampY+1][lampX+1] = 'o'
	if glow > 0 {
		ring := byte('g')
		if glow > 1 {
			ring = 'o'
		}
		c[lampY+1][lampX+1] = 'W'
		c.art(glowArt, lampX-1, lampY-1, ring)
	}
}

// wheel draws wheel n (0 the first, on the left) standing on digit d,
// the rows at its top and foot showing the numbers either side of it
// turning away round the wheel.
func (c *canvas) wheel(n, d int) {
	x0, y0 := caseX+1+n*8, caseY+1
	c.well(x0, y0)
	ink := wheelInk[n == wheels-1]
	for k := -1; k <= 1; k++ {
		top := y0 + 2 + k*pitch
		for r, row := range digitArt[(d+k+10)%10] {
			y := top + r
			if y < y0 || y > y0+10 {
				continue
			}
			// the wheel's curve: a number dims as it turns away
			c0 := ink[0]
			if y == y0 || y == y0+10 {
				c0 = ink[2]
			} else if y == y0+1 || y == y0+9 {
				c0 = ink[1]
			}
			for j := range len(row) {
				if row[j] == '#' {
					c[y][x0+1+j] = c0
				}
			}
		}
	}
}

// letter puts a K or an M on wheel n, standing.
func (c *canvas) letter(n int, g []string) {
	x0, y0 := caseX+1+n*8, caseY+1
	c.well(x0, y0)
	c.art(g, x0+1, y0+2, wheelInk[n == wheels-1][0])
}

func (c *canvas) well(x0, y0 int) {
	for y := y0; y < y0+11; y++ {
		for x := x0; x < x0+7; x++ {
			c[y][x] = 'k'
		}
	}
}

// bars draws the day: px a bar, the one at cur still filling.
func (c *canvas) bars(bars [12]int, cur int) {
	for i, h := range bars {
		x0 := barsX + i*3
		fill, top := byte('L'), byte('I')
		if i == cur {
			fill, top = 'o', 'O'
		}
		for k := range h {
			for j := range 2 {
				ink := fill
				if k == h-1 {
					ink = top
				}
				c[barsBase-k][x0+j] = ink
			}
		}
	}
}

// digitsOf is a count on n wheels, the first on the left, nought-padded;
// a count too long for them shows its last n digits, as a counter that
// went round does.
func digitsOf(v int64, n int) []int {
	out := make([]int, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = int(v % 10)
		v /= 10
	}
	return out
}

// counterDay is what a Hit Counter shows.
type counterDay struct {
	Total int64     // page views since the site was added
	Bars  [12]int64 // today's, two hours a bar from midnight
	Now   int       // the bar the clock is in
	Top   int64     // the views a full bar stands for: the busiest two hours of today and yesterday
}

// counterFrame is one picture of the loop.
type counterFrame struct {
	wheels []int    // the digit wheels, the first on the left
	letter []string // a K or an M on the last wheel, or none
	bars   [12]int  // px
	cur    int      // the bar the clock is in, still filling
	glow   int      // the lamp: 0 at rest, 1 rising, 2 full
	cs     int      // how long it stands, in hundredths of a second
}

// unitOf is what the wheels count in: views while five wheels hold the
// count; past that thousands on four, the last wheel a K; then millions.
func unitOf(total int64) (unit int64, digits int, letter []string) {
	switch {
	case total < 100000:
		return 1, wheels, nil
	case total < 10000000:
		return 1000, wheels - 1, kArt
	}
	return 1000000, wheels - 1, mArt
}

// breath is the lamp's: a rest, then the glow up through the dim ring to
// the full one and down again.
var breath = []struct{ glow, cs int }{{0, 240}, {1, 12}, {2, 50}, {1, 12}}

// counterFrames is the loop for a day: the numbers as they stand, and
// the lamp breathing over them.
func counterFrames(d counterDay) []counterFrame {
	unit, n, letter := unitOf(d.Total)
	// the wheels say what has been reached, never ahead of it
	f := counterFrame{wheels: digitsOf(d.Total/unit, n), letter: letter, cur: d.Now}
	for i, v := range d.Bars {
		if v > 0 && d.Top > 0 {
			// a bar with any views shows, however few
			f.bars[i] = max(1, min(barTop, int(math.Round(float64(v)*barTop/float64(d.Top)))))
		}
	}
	out := make([]counterFrame, len(breath))
	for i, b := range breath {
		f.glow, f.cs = b.glow, b.cs
		out[i] = f
	}
	return out
}

// draw is a frame's picture.
func (f counterFrame) draw() *canvas {
	c := counterFace()
	c.lamp(f.glow)
	for i, d := range f.wheels {
		c.wheel(i, d)
	}
	if f.letter != nil {
		c.letter(wheels-1, f.letter)
	}
	c.bars(f.bars, f.cur)
	return c
}

// drawCounter is a day's Hit Counter as a GIF. After the first frame,
// each holds only the box that changed, drawn over the one before.
func drawCounter(d counterDay) []byte {
	g := &gif.GIF{Config: image.Config{ColorModel: counterPal, Width: cw, Height: ch}}
	var prev *canvas
	for _, f := range counterFrames(d) {
		c := f.draw()
		r := image.Rect(0, 0, cw, ch)
		if prev != nil {
			if r = changed(prev, c); r.Empty() {
				g.Delay[len(g.Delay)-1] += f.cs // the same picture: it stands longer
				continue
			}
		}
		im := image.NewPaletted(r, counterPal)
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				im.SetColorIndex(x, y, counterIndex[c[y][x]])
			}
		}
		g.Image = append(g.Image, im)
		g.Delay = append(g.Delay, f.cs)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
		prev = c
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		panic("counter: " + err.Error()) // a drawing of our own, in its own palette
	}
	return b.Bytes()
}

// changed is the box round every pixel that differs.
func changed(a, b *canvas) image.Rectangle {
	var r image.Rectangle
	for y := range ch {
		for x := range cw {
			if a[y][x] != b[y][x] {
				r = r.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	return r
}
