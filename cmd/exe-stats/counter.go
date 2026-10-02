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
// The first frame is the badge as it stands, and it is held, so a reader
// who stops animations has the whole of it. Then the day replays:
// midnight slides it off the chart and turns the wheels back to where the
// day began, and it fills again two hours a beat, the lamp lit while
// readers arrive, until the wheels slow onto the count the loop began on.
// A site with no views yet today stands still.
//
// The face, the glyphs and the palette are badge.py's, in its order; the
// test draws badge.gif's first frame here and compares the two.

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

// lamp is the live lamp: lit while a reader arrives, a ring round it.
func (c *canvas) lamp(lit bool) {
	c.art(lampArt, lampX, lampY, 'O')
	c[lampY+1][lampX+1] = 'o'
	if lit {
		c[lampY+1][lampX+1] = 'W'
		c.art(glowArt, lampX-1, lampY-1, 'o')
	}
}

// wheel draws wheel n (0 the first, on the left) turned to pos: 7.3 is
// seven with eight a third of the way up. A wheel spinning is a blur:
// its numbers dim, and a ghost of the last half turn is left behind them.
func (c *canvas) wheel(n int, pos float64, blur bool) {
	x0, y0 := caseX+1+n*8, caseY+1
	c.well(x0, y0)
	ink := wheelInk[n == wheels-1]
	on, turning, away := ink[0], ink[1], ink[2]
	type layer struct {
		p      float64
		c0, c1 byte // the face, and the rows turning away
		c2     byte // the rows at the wheel's top and foot
	}
	layers := []layer{{pos, on, turning, away}}
	if blur {
		layers = []layer{{pos - 0.5, away, away, away}, {pos, turning, away, away}}
	}
	for _, l := range layers {
		d := math.Floor(l.p)
		off := int(math.RoundToEven((l.p - d) * pitch))
		for k := -1; k <= 1; k++ {
			top := y0 + 2 - off + k*pitch
			for r, row := range digitArt[((int(d)+k)%10+10)%10] {
				y := top + r
				if y < y0 || y > y0+10 {
					continue
				}
				// the wheel's curve: a number dims as it turns away
				ink := l.c0
				if y == y0 || y == y0+10 {
					ink = l.c2
				} else if y == y0+1 || y == y0+9 {
					ink = l.c1
				}
				for j := range len(row) {
					if row[j] == '#' {
						c[y][x0+1+j] = ink
					}
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

// bars draws the day: px a bar, the one at cur still filling; shift
// slides the whole day left, out past the chart's edge.
func (c *canvas) bars(bars [12]int, cur, shift int) {
	for i, h := range bars {
		x0 := barsX + i*3 - shift
		fill, top := byte('L'), byte('I')
		if i == cur {
			fill, top = 'o', 'O'
		}
		for k := range h {
			for j := range 2 {
				if x0+j >= barsX {
					ink := fill
					if k == h-1 {
						ink = top
					}
					c[barsBase-k][x0+j] = ink
				}
			}
		}
	}
}

// positions are where each of n wheels stands for a count, odometer
// fashion: the last wheel turns with the count, and every other one only
// while all the wheels to its right go from nine to nought. Counted in
// numbers passed, not taken round ten, so a frame can tell how far each
// went.
func positions(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range n {
		p := math.Pow(10, float64(n-1-i))
		if p == 1 {
			out[i] = v
		} else {
			out[i] = math.Floor(v/p) + math.Max(0, math.Mod(v, p)-(p-1))
		}
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
	wheels []float64 // the digit wheels' positions
	blur   []bool
	bars   [12]int // px
	cur    int     // the bar still filling
	lit    bool
	shift  int
	cs     int // how long it stands, in hundredths of a second
}

// unitOf is what the wheels count in: views while five wheels hold the
// count; past that thousands on four, the last wheel a K; then millions.
func unitOf(total int64) (unit float64, digits int, letter []string) {
	switch {
	case total < 100000:
		return 1, wheels, nil
	case total < 10000000:
		return 1000, wheels - 1, kArt
	}
	return 1000000, wheels - 1, mArt
}

// counterFrames is the loop for a day.
func counterFrames(d counterDay) []counterFrame {
	unit, n, _ := unitOf(d.Total)
	pos := func(v float64) []float64 {
		if unit > 1 {
			v = math.Floor(v / unit) // the wheels say what has been reached, never ahead of it
		}
		return positions(v, n)
	}
	px := func(views float64) int {
		if views <= 0 || d.Top <= 0 {
			return 0
		}
		return max(1, min(barTop, int(math.Round(views*barTop/float64(d.Top)))))
	}
	var whole [12]int
	var today int64
	for i, v := range d.Bars {
		whole[i] = px(float64(v))
		today += v
	}

	var out []counterFrame
	var last []float64
	show := func(f counterFrame) {
		if f.blur == nil {
			f.blur = make([]bool, n)
			for i := range f.wheels {
				// a wheel that went more than two numbers since the last
				// frame is a blur
				f.blur[i] = last != nil && f.wheels[i]-last[i] > 2
			}
		}
		last = f.wheels
		out = append(out, f)
	}

	// the badge as it stands
	show(counterFrame{wheels: pos(float64(d.Total)), bars: whole, cur: d.Now, cs: 400})
	if today == 0 {
		return out
	}

	// midnight: the day slides off the chart, and the wheels turn on to
	// where it began and stop there one after another, the last first
	start := float64(max(0, d.Total-today))
	from, to := pos(float64(d.Total)), pos(start)
	need := make([]float64, n)
	steps := 4 // the slide: nine px a frame
	for i := range n {
		need[i] = math.Mod(math.Mod(to[i]-from[i], 10)+10, 10)
		if need[i] > 0 {
			steps = max(steps, n+1-i)
		}
	}
	for s := 1; s <= steps; s++ {
		ws, spin := make([]float64, n), make([]bool, n)
		for i := range n {
			stop := float64(n + 1 - i) // the last wheel stops on the second frame
			ws[i] = from[i] + need[i]*min(1, float64(s)/stop)
			spin[i] = need[i] > 0 && float64(s) < stop
		}
		show(counterFrame{wheels: ws, blur: spin, bars: whole, cur: d.Now, shift: min(36, s*9), cs: 4})
	}
	show(counterFrame{wheels: to, cur: -1, cs: 30}) // a new day

	// the day again, two hours a beat
	var bars [12]int
	seen := start
	for i := range d.Now {
		half := float64(d.Bars[i]) / 2
		bars[i] = px(half)
		show(counterFrame{wheels: pos(seen + half), bars: bars, cur: i, lit: true, cs: 5})
		seen += float64(d.Bars[i])
		bars[i] = whole[i]
		show(counterFrame{wheels: pos(seen), bars: bars, cur: i + 1, lit: true, cs: 5})
	}
	// and the two hours the clock is in, slowing the way a wheel does
	// onto the count the loop opened on: the frame after the last of
	// these is the first
	rest := float64(d.Bars[d.Now])
	gaps := []struct {
		views float64
		cs    int
	}{{rest / 2, 5}, {72, 5}, {32, 5}, {14, 6}, {6, 6}, {3, 7}, {1.4, 8}, {0.5, 10}}
	for i, g := range gaps {
		if i > 0 {
			g.views *= unit
		}
		if g.views >= rest || (i == 0 && g.views <= 72*unit) {
			continue
		}
		bars[d.Now] = px(rest - g.views)
		show(counterFrame{wheels: pos(float64(d.Total) - g.views), bars: bars, cur: d.Now, lit: true, cs: g.cs})
	}
	return out
}

// draw is a frame's picture.
func (f counterFrame) draw(letter []string) *canvas {
	c := counterFace()
	c.lamp(f.lit)
	for i, p := range f.wheels {
		c.wheel(i, p, f.blur[i])
	}
	if letter != nil {
		c.letter(wheels-1, letter)
	}
	c.bars(f.bars, f.cur, f.shift)
	return c
}

// drawCounter is a day's Hit Counter as a GIF. After the first frame,
// each holds only the box that changed, drawn over the one before.
func drawCounter(d counterDay) []byte {
	_, _, letter := unitOf(d.Total)
	g := &gif.GIF{Config: image.Config{ColorModel: counterPal, Width: cw, Height: ch}}
	var prev *canvas
	for _, f := range counterFrames(d) {
		c := f.draw(letter)
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
