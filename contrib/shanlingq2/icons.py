#!/usr/bin/env python3
"""Draws icon.png (52 px) and icon-40.png (40 px), Streaming's row icon, from Jellyfin's
icon-transparent.svg (https://github.com/jellyfin/jellyfin-ux, CC BY-SA 4.0): its two paths and
its purple-to-blue gradient, on a clear background, drawn 8x and scaled down. Needs Pillow.
Q2 Pod greys the icon itself in the Minimal theme. It fits icon-transparent.svg only: the gradient
and the logo's height below are copied from it, and other path commands are refused.

    python3 contrib/shanlingq2/icons.py path/to/icon-transparent.svg
"""
import re
import sys
from pathlib import Path

from PIL import Image, ImageChops, ImageDraw

SS = 8  # supersampling
STOPS = ((0xAA, 0x5C, 0xC3), (0x00, 0xA4, 0xDC))
G0, G1 = (110.25, 213.3), (496.14, 436.09)  # the gradient's line, in the 512 viewBox
TOP, BOTTOM = 23.3, 489.0  # the logo's height in the viewBox, to centre it


def subpaths(d):
    """Flattens an SVG path (M C S L Z, either case) into polygons."""
    if re.search(r'[^MmCcSsLlZz0-9.,\s-]', d):
        sys.exit(f'unsupported path data: {d[:40]}')
    toks = re.findall(r'[MmCcSsLlZz]|-?(?:\d+\.?\d*|\.\d+)', d)
    polys, cur, p, start, ctrl, i, cmd = [], [], (0.0, 0.0), (0.0, 0.0), None, 0, None
    def num():
        nonlocal i
        i += 1
        return float(toks[i - 1])
    def cubic(c1, c2, e):
        for k in range(1, 17):
            t = k / 16; u = 1 - t
            cur.append(tuple(u**3 * a + 3 * u * u * t * b + 3 * u * t * t * c + t**3 * f
                             for a, b, c, f in zip(p, c1, c2, e)))
    while i < len(toks):
        if toks[i].isalpha():
            cmd = toks[i]; i += 1
        rel = cmd.islower()
        o = p if rel else (0.0, 0.0)
        pt = lambda: (o[0] + num(), o[1] + num())
        if cmd in 'Zz':
            if cur: polys.append(cur)
            cur, p, ctrl = [], start, None
        elif cmd in 'Mm':
            if cur: polys.append(cur)
            p = start = pt(); cur = [p]; ctrl = None; cmd = 'l' if rel else 'L'
        elif cmd in 'Ll':
            p = pt(); cur.append(p); ctrl = None
        elif cmd in 'Cc':
            c1, c2, e = pt(), pt(), pt(); cubic(c1, c2, e); ctrl, p = c2, e
        elif cmd in 'Ss':
            c1 = (2 * p[0] - ctrl[0], 2 * p[1] - ctrl[1]) if ctrl else p
            c2, e = pt(), pt(); cubic(c1, c2, e); ctrl, p = c2, e
    if cur: polys.append(cur)
    return polys


def draw(svg, size):
    big = size * SS
    scale = (big - 2 * SS) / (BOTTOM - TOP)  # 1 px clear at top and bottom
    dx, dy = big / 2 - 256 * scale, SS - TOP * scale
    mask = Image.new('L', (big, big), 0)
    paths = re.findall(r'\sd="([^"]+)"', svg)
    if not paths:
        sys.exit('no paths in the SVG')
    for d in paths:
        for poly in subpaths(d):  # even-odd: each ring flips what it covers
            m = Image.new('L', (big, big), 0)
            ImageDraw.Draw(m).polygon([(x * scale + dx, y * scale + dy) for x, y in poly], fill=255)
            mask = ImageChops.logical_xor(mask.convert('1'), m.convert('1')).convert('L')
    vx, vy = G1[0] - G0[0], G1[1] - G0[1]
    colour = Image.new('RGB', (big, big))
    px = colour.load()
    for y in range(big):
        for x in range(big):
            sx, sy = (x + 0.5 - dx) / scale, (y + 0.5 - dy) / scale
            t = min(1.0, max(0.0, ((sx - G0[0]) * vx + (sy - G0[1]) * vy) / (vx * vx + vy * vy)))
            px[x, y] = tuple(round(a + (b - a) * t) for a, b in zip(*STOPS))
    colour.putalpha(mask)
    return colour.resize((size, size), Image.LANCZOS)


if __name__ == '__main__':
    svg = Path(sys.argv[1]).read_text(encoding='utf-8')
    here = Path(__file__).resolve().parent
    for name, size in (('icon.png', 52), ('icon-40.png', 40)):
        draw(svg, size).save(here / name, optimize=True)
