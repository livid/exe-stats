#!/usr/bin/env python3
"""The stats server's 88x31 badge, drawn pixel by pixel.

    python3 badge.py                  writes badge.gif beside this file
    python3 badge.py sheet.png        also writes every frame, 4x, to look at

The web's first analytics was the hit counter, a row of number wheels at
the foot of a page, so the badge is one: five wheels in a sunken window,
the last one in the orange the desk gives visitors, beside a day's page
views in the desk's own bars, under a band in its highlight indigo with
the name and a live lamp. The loop is a day. It opens in the evening on
01997 (the first frame is the whole badge, for a reader who stops
animations); readers arrive one at a time, the lamp lights for each, and
01999 carries into 02000, four wheels turning at once. Then the evening
runs fast, midnight slides the day off the chart and spins the wheels on
to nought, and the next day fills in until the counter slows onto 01997
again, the frame the loop began on. Needs Pillow; nothing else.
"""
import math
import os
import sys

from PIL import Image

W, H = 88, 31

PAL = {
    'K': (0x26, 0x26, 0x26),  # the Platinum black: the frame and the counter's case
    'k': (0x00, 0x00, 0x00),  # the wheels
    'W': (0xff, 0xff, 0xff),  # the face, the name and the numbers
    'I': (0x33, 0x33, 0x99),  # the highlight indigo: the band, and a bar's top
    'i': (0x5c, 0x5c, 0xb8),  # the band catching the light
    'L': (0x99, 0x99, 0xff),  # a bar, in the desk's deeper lavender
    'O': (0xff, 0x66, 0x00),  # the visitors' orange: the last wheel, the lamp, the bar still filling
    'o': (0xff, 0xcc, 0x99),  # the bar still filling, as the desk draws it
    'r': (0xa8, 0x44, 0x00),  # the orange turning away on its wheel
    'R': (0x55, 0x22, 0x00),  # and further
    'S': (0x99, 0x99, 0x99),  # the window's sunken edge
    'd': (0x55, 0x55, 0x55),  # between the wheels
    'b': (0xbb, 0xbb, 0xbb),  # a number turning away
    'm': (0x77, 0x77, 0x77),  # and further
}
ORDER = list(PAL)

# the name: two-pixel stems on a six-row x-height, the t two rows taller
NAME = {
    'e': [".####.", "##..##", "######", "##....", "##...#", ".####."],
    'x': ["##..##", "##..##", ".####.", ".####.", "##..##", "##..##"],
    '-': ["....", "....", "####", "....", "....", "...."],
    's': [".#####", "##....", ".####.", "....##", "....##", "#####."],
    't': [".##..", ".##..", "#####", ".##..", ".##..", ".##..", ".##..", "..###"],
    'a': [".####.", "....##", ".#####", "##..##", "##..##", ".#####"],
}
NX, NBASE = 4, 10  # the name's left edge, and the row under its x-height

LAMP = [".###.", "#####", "#####", "#####", ".###."]
LX, LY = 80, 4  # its top left
GLOW = ["..###..", ".#...#.", "#.....#", "#.....#", "#.....#", ".#...#.", "..###.."]

# the wheels' numbers, five by seven
DIGITS = [
    [".###.", "#...#", "#...#", "#...#", "#...#", "#...#", ".###."],
    ["..#..", ".##..", "..#..", "..#..", "..#..", "..#..", ".###."],
    [".###.", "#...#", "....#", "...#.", "..#..", ".#...", "#####"],
    [".###.", "#...#", "....#", "..##.", "....#", "#...#", ".###."],
    ["...#.", "..##.", ".#.#.", "#..#.", "#####", "...#.", "...#."],
    ["#####", "#....", "####.", "....#", "....#", "#...#", ".###."],
    ["..##.", ".#...", "#....", "####.", "#...#", "#...#", ".###."],
    ["#####", "....#", "...#.", "..#..", ".#...", ".#...", ".#..."],
    [".###.", "#...#", "#...#", ".###.", "#...#", "#...#", ".###."],
    [".###.", "#...#", "#...#", ".####", "....#", "...#.", ".##.."],
]
WHEELS = 5
CX, CY = 4, 15  # the counter's case, 41 by 13, top left
PITCH = 10  # a number and the gap to the next, round the wheel
INK = {False: ('W', 'b', 'm'), True: ('O', 'r', 'R')}  # face on, turning, turned away

# a day's page views in two-hour bars, as px: quiet small hours, a
# morning climb, the afternoon's top, a long evening
DAY = [4, 3, 2, 3, 6, 9, 11, 12, 11, 10, 8, 6]
BX, BASE = 49, 26  # the first bar's left edge, and the bars' bottom row
NOW, NOW_PX = 9, 5  # the first frame: the 18:00 bar, five px into it
OPEN = 1997  # and the counter then
SCALE = OPEN / (sum(DAY[:NOW]) + NOW_PX)  # page views a px


def face():
    img = [['W'] * W for _ in range(H)]
    for x in range(W):
        img[0][x] = img[H - 1][x] = 'K'
        img[11][x] = 'K'  # the band's foot
    for y in range(H):
        img[y][0] = img[y][W - 1] = 'K'
    for y in range(1, 11):
        for x in range(1, W - 1):
            img[y][x] = 'i' if y == 1 else 'I'
    x = NX
    for ch in "exe-stats":
        g = NAME[ch]
        for r, row in enumerate(g):
            for j, c in enumerate(row):
                if c == '#':
                    img[NBASE - len(g) + r][x + j] = 'W'
        x += len(g[0]) + 1
    # the counter's case, sunk into the face, and the chart's floor
    for x in range(CX - 1, CX + WHEELS * 8 + 1):
        img[CY - 1][x] = 'S'
    for y in range(CY - 1, CY + 13):
        img[y][CX - 1] = 'S'
    for y in range(CY, CY + 13):
        for x in range(CX, CX + WHEELS * 8 + 1):
            edge = y in (CY, CY + 12) or x in (CX, CX + WHEELS * 8)
            img[y][x] = 'K' if edge else 'd'
    for x in range(BX - 1, BX + len(DAY) * 3):
        img[BASE + 1][x] = 'K'
    return img


def draw_lamp(img, lit):
    """The live lamp: lit while a reader arrives, a ring round it."""
    for r, row in enumerate(LAMP):
        for j, c in enumerate(row):
            if c == '#':
                img[LY + r][LX + j] = 'O'
    img[LY + 1][LX + 1] = 'W' if lit else 'o'
    if lit:
        for r, row in enumerate(GLOW):
            for j, c in enumerate(row):
                if c == '#':
                    img[LY - 1 + r][LX - 1 + j] = 'o'


def draw_wheel(img, n, pos, blur):
    """Wheel n (0 the first, on the left) turned to pos, a float: 7.3 is
    seven with eight a third of the way up. A wheel spinning is a blur:
    its numbers dim, and a ghost of the last half turn is left behind
    them."""
    x0, y0 = CX + 1 + n * 8, CY + 1
    for y in range(y0, y0 + 11):
        for x in range(x0, x0 + 7):
            img[y][x] = 'k'
    on, turning, away = INK[n == WHEELS - 1]
    layers = [(pos, (on, turning, away))]
    if blur:
        layers = [(pos - 0.5, (away, away, away)), (pos, (turning, away, away))]
    for p, (c0, c1, c2) in layers:
        d = math.floor(p)
        off = round((p - d) * PITCH)
        for k in (-1, 0, 1):
            top = y0 + 2 - off + k * PITCH
            for r, row in enumerate(DIGITS[(d + k) % 10]):
                y = top + r
                if not y0 <= y <= y0 + 10:
                    continue
                # the wheel's curve: a number dims as it turns away
                c = c2 if y in (y0, y0 + 10) else c1 if y in (y0 + 1, y0 + 9) else c0
                for j, v in enumerate(row):
                    if v == '#':
                        img[y][x0 + 1 + j] = c


def positions(v):
    """Where each wheel stands for a count, odometer fashion: the last
    wheel turns with the count, and every other one only while all the
    wheels to its right go from nine to nought. Counted in numbers
    passed, not taken round ten, so a frame can tell how far each went."""
    out = []
    for n in range(WHEELS):
        p = 10 ** (WHEELS - 1 - n)
        out.append(v if p == 1 else math.floor(v / p) + max(0.0, v % p - (p - 1)))
    return out


def draw_bars(img, bars, cur, shift=0):
    """bars: px a bar, the one at cur still filling; shift slides the
    whole day left, out past the chart's edge."""
    for i, h in enumerate(bars):
        x0 = BX + i * 3 - shift
        fill, top = ('o', 'O') if i == cur else ('L', 'I')
        for k in range(h):
            for j in range(2):
                if x0 + j >= BX:
                    img[BASE - k][x0 + j] = top if k == h - 1 else fill


def frame(wheels, blur, bars, cur, lit, shift=0):
    img = face()
    draw_lamp(img, lit)
    for n, p in enumerate(wheels):
        draw_wheel(img, n, p, blur[n])
    draw_bars(img, bars, cur, shift)
    return img


def day_until(px):
    """The day's bars px into it, and which one is filling."""
    bars, left = [], px
    for h in DAY:
        bars.append(min(h, max(0, round(left))))
        left -= h
    cur = next((i for i in range(len(DAY)) if sum(DAY[:i + 1]) > px + 1e-9), len(DAY) - 1)
    return bars, cur


def timeline():
    """(frame, centiseconds) for one loop."""
    out = []
    last = [None]

    def show(v, lit, cs, wheels=None, spin=None, shift=0, day=None):
        """A frame at count v (the bars follow it) or with the wheels,
        which of them spin, and the day given outright."""
        ws = wheels if wheels is not None else positions(v)
        prev = last[0] or ws
        # a wheel that went more than two numbers since the last frame is
        # a blur; midnight's own wheels say for themselves
        blur = spin or [a - b > 2 for a, b in zip(ws, prev)]
        last[0] = ws
        bars, cur = day if day else day_until(v / SCALE)
        out.append((frame(ws, blur, bars, cur, lit, shift), cs))

    def hit(v, hold):
        """One reader: the wheels roll a number on while the lamp is lit."""
        for f in (0.3, 0.6, 0.9):
            show(v + f, True, 3)
        show(v + 1, False, hold)

    # the evening, as the badge opens: 01997, then four readers
    show(OPEN, False, 120)
    hit(OPEN, 45)
    hit(OPEN + 1, 55)
    hit(OPEN + 2, 100)  # 01999 to 02000: four wheels go round together
    hit(OPEN + 3, 20)

    # the rest of the evening, fast, to midnight
    v0, px0, px1 = OPEN + 4, OPEN / SCALE, sum(DAY)
    for s in range(1, 8):
        show(round(v0 + (px1 - px0) * SCALE * s / 7), True, 5)
    full = round(v0 + (px1 - px0) * SCALE)
    # the day is whole: its last bar turns from filling to filled
    whole = (list(DAY), -1)
    show(full, False, 40, day=whole)

    # midnight: the day slides off the chart, and the wheels spin on to
    # nought and stop there one after another, the last wheel first
    start = [math.floor(p) for p in positions(full)]
    need = [-p % 10 for p in start]
    for s in range(1, max(WHEELS + 1 - n for n, k in enumerate(need) if k)):
        ws, spin = [], []
        for n, (p, k) in enumerate(zip(start, need)):
            stop = WHEELS + 1 - n  # the last wheel stops on the second frame
            ws.append(p + k * min(1.0, s / stop))
            spin.append(k > 0 and s < stop)
        show(None, False, 4, wheels=ws, spin=spin, shift=min(36, s * 9), day=whole)
    show(0, False, 45)  # the last of them stops: a new day

    # the next day, two hours a beat, quiet then busy
    for i in range(NOW):
        for px in (sum(DAY[:i]) + DAY[i] / 2, sum(DAY[:i + 1])):
            show(round(px * SCALE), True, 5)
    # and into the evening, slowing the way a wheel does onto the count
    # the loop opened on: the frame after the last of these is the first
    for gap, cs in ((72, 5), (32, 5), (14, 6), (6, 6), (3, 7), (1.4, 8), (0.5, 10)):
        show(OPEN - gap, True, cs)
    return out


def to_image(f):
    im = Image.new('P', (W, H))
    flat = []
    for c in ORDER:
        flat.extend(PAL[c])
    im.putpalette(flat)
    im.putdata([ORDER.index(c) for row in f for c in row])
    return im


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    frames = timeline()
    ims = [to_image(f) for f, _ in frames]
    ims[0].save(os.path.join(here, 'badge.gif'), save_all=True, append_images=ims[1:],
                duration=[cs * 10 for _, cs in frames], loop=0, disposal=1, optimize=False)
    if len(sys.argv) > 1:
        scale, cols = 4, 4
        rows = (len(ims) + cols - 1) // cols
        sheet = Image.new('RGB', (cols * (W * scale + 8), rows * (H * scale + 8)), (0x55, 0x55, 0x55))
        for i, im in enumerate(ims):
            big = im.convert('RGB').resize((W * scale, H * scale), Image.NEAREST)
            sheet.paste(big, ((i % cols) * (W * scale + 8), (i // cols) * (H * scale + 8)))
        sheet.save(sys.argv[1])
    total = sum(cs for _, cs in frames)
    print(f"{len(frames)} frames, {total / 100:.1f} s a loop, "
          f"{os.path.getsize(os.path.join(here, 'badge.gif'))} bytes")


if __name__ == '__main__':
    main()
