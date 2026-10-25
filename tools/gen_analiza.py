"""Render static/gen-analiza.gif: GEN, the Transgenia mascot, analysing a
meeting. Pure Pillow (no SVG renderer on the server). GEN is drawn from the
official geometry of GEN-solida.svg (viewBox 120x120): shell and face are the
same hexagons with round joins (stroke drawn as segments plus round caps),
eyes, smile (quadratic curve) and node. `--check REF.png` renders the neutral
pose at 600 px and compares it with the official PNG.

Scene (authorised by Saurat on 2026-10-15 to go beyond the four primitives):
audio bars on the left, transcript lines appearing in the middle while GEN
reads (eyes scan), blinks and floats; each processed line pulses the node with
a ring; at the end GEN turns 60 degrees and back, lands with a slight squash,
and the cycle restarts. Palette only: #101820 #EE7623 #D1333B #FEFEEA #3D3935
on white.

Usage: python3 -I gen_analiza.py OUT.gif [--check REF.png]
"""
import math
import sys

from PIL import Image, ImageChops, ImageDraw

NAVY, ORANGE, CRIMSON, CREAM, WARM, WHITE = "#101820", "#EE7623", "#D1333B", "#FEFEEA", "#3D3935", "#FFFFFF"
SHELL = [(60, 18), (96.37, 39), (96.37, 81), (60, 102), (23.63, 81), (23.63, 39)]
FACE = [(60, 36), (80.78, 48), (80.78, 72), (60, 84), (39.22, 72), (39.22, 48)]
EYES = [(51.5, 56), (68.5, 56)]
NODE = (60, 21)


def quad(p0, p1, p2, n=24):
    return [((1 - t) ** 2 * p0[0] + 2 * (1 - t) * t * p1[0] + t * t * p2[0],
             (1 - t) ** 2 * p0[1] + 2 * (1 - t) * t * p1[1] + t * t * p2[1]) for t in (i / n for i in range(n + 1))]


def round_poly(d, pts, color, width):
    """Filled polygon with stroke-linejoin=round of the same colour."""
    d.polygon(pts, fill=color)
    r = width / 2
    for a, b in zip(pts, pts[1:] + pts[:1]):
        d.line([a, b], fill=color, width=max(1, round(width)))
    for x, y in pts:
        d.ellipse([x - r, y - r, x + r, y + r], fill=color)


def round_line(d, pts, color, width):
    d.line(pts, fill=color, width=max(1, round(width)), joint="curve")
    r = width / 2
    for x, y in (pts[0], pts[-1]):
        d.ellipse([x - r, y - r, x + r, y + r], fill=color)


def draw_gen(d, cx, cy, size, angle=0.0, sx=1.0, sy=1.0, eye_dx=0.0, blink=0.0, node_scale=1.0):
    """GEN centred at (cx, cy), `size` px for the 120-unit viewBox."""
    k = size / 120
    ca, sa = math.cos(math.radians(angle)), math.sin(math.radians(angle))

    def tf(p):
        x, y = p[0] - 60, p[1] - 60
        x, y = x * ca - y * sa, x * sa + y * ca
        return (cx + x * k * sx, cy + y * k * sy)

    round_poly(d, [tf(p) for p in SHELL], NAVY, 14 * k)
    round_poly(d, [tf(p) for p in FACE], CREAM, 10 * k)
    for ex, ey in EYES:
        x, y = tf((ex + eye_dx, ey))
        r = 4.4 * k
        h = r * max(0.12, 1 - blink)
        d.ellipse([x - r, y - h, x + r, y + h], fill=NAVY)
    round_line(d, [tf(p) for p in quad((51, 68), (60, 77), (69, 68))], CRIMSON, 4 * k)
    nx, ny = tf(NODE)
    r = 5.5 * k * node_scale
    d.ellipse([nx - r, ny - r, nx + r, ny + r], fill=ORANGE)
    return tf(NODE), k


def ease(t):  # cubic-bezier(.65,0,.35,1) approximation
    return 4 * t ** 3 if t < .5 else 1 - (-2 * t + 2) ** 3 / 2


def frame(t, W, H, S):
    """Render the scene at time t (0..1 of the loop) at S times the display size."""
    img = Image.new("RGB", (W * S, H * S), WHITE)
    d = ImageDraw.Draw(img)
    u = S  # one display pixel

    # Audio bars (voice levels) on the left.
    for i in range(4):
        level = 0.35 + 0.65 * abs(math.sin(2 * math.pi * (t * 3 + i * 0.27)) * math.sin(2 * math.pi * (t * 5 + i * 0.13) + 1))
        h = (8 + 22 * level) * u
        x = (6 + i * 7) * u
        cy = H / 2 * u
        round_line(d, [(x, cy - h / 2), (x, cy + h / 2)], ORANGE if i % 2 == 0 else CRIMSON, 4 * u)

    # Transcript lines appearing one by one (0..0.75), held, then retracting.
    lengths = [44, 34, 40]
    for i, L in enumerate(lengths):
        start = 0.08 + i * 0.2
        grow = min(1, max(0, (t - start) / 0.14))
        if t > 0.86:
            grow *= max(0, 1 - (t - 0.86) / 0.1)
        if grow <= 0:
            continue
        y = (H / 2 - 13 + i * 13) * u
        x0 = 40 * u
        round_line(d, [(x0, y), (x0 + L * grow * u, y)], WARM, 4 * u)

    # GEN on the right.
    size = 64
    cx, cy = (W - size / 2 - 6) * u, H / 2 * u
    bob = math.sin(2 * math.pi * t * 2) * 1.6 * u
    eye_dx = 0.0
    if t < 0.72:  # reading: eyes scan left as each line appears, return right
        phase = ((t - 0.08) / 0.2) % 1 if t >= 0.08 else 0
        eye_dx = -3.0 * math.sin(math.pi * phase)
    blink = 0.0
    for b in (0.30, 0.62):
        if b <= t < b + 0.035:
            blink = 1 - abs((t - b) / 0.0175 - 1)
    node_scale, rings = 1.0, []
    for i in range(3):  # a pulse each time a line completes
        p = (t - (0.22 + i * 0.2)) / 0.075
        if 0 <= p <= 1:
            node_scale = 1 + 0.35 * math.sin(math.pi * p)
        if 0 <= p <= 2:
            rings.append(p / 2)
    angle, sx, sy = 0.0, 1.0, 1.0
    if 0.74 <= t < 0.80:
        angle = 60 * ease((t - 0.74) / 0.06)
    elif 0.80 <= t < 0.86:
        angle = 60 * (1 - ease((t - 0.80) / 0.06))
    if 0.86 <= t < 0.92:  # landing squash
        q = math.sin(math.pi * (t - 0.86) / 0.06)
        sx, sy = 1 + 0.06 * q, 1 - 0.07 * q
    (nx, ny), k = draw_gen(d, cx, cy + bob, size * u, angle, sx, sy, eye_dx, blink, node_scale)
    for r in rings:
        rad = (6 + 14 * r) * k  # viewBox units around the node
        w = max(1, round((2.2 - 1.8 * r) * u))
        d.ellipse([nx - rad, ny - rad, nx + rad, ny + rad], outline=ORANGE, width=w)
    return img


def render_gif(path, W=168, H=84, scale=2, ss=4, frames=40, ms=100):
    imgs = []
    for f in range(frames):
        big = frame(f / frames, W, H, scale * ss)
        imgs.append(big.resize((W * scale, H * scale), Image.LANCZOS))
    # Fixed palette: the exact brand colours first, then the edge blends
    # between colours that actually touch, so solid areas stay exact and only
    # anti-aliased edges use intermediate tones.
    hexes = [WHITE, NAVY, ORANGE, CRIMSON, CREAM, WARM]
    rgb = [tuple(int(h[i:i + 2], 16) for i in (1, 3, 5)) for h in hexes]
    named = dict(zip(hexes, rgb))
    pairs = [(WHITE, NAVY), (WHITE, ORANGE), (WHITE, CRIMSON), (WHITE, WARM), (NAVY, CREAM),
             (NAVY, ORANGE), (CREAM, CRIMSON), (CREAM, NAVY)]
    colours = list(rgb)
    for a, b in pairs:
        for f in (0.2, 0.4, 0.6, 0.8):
            colours.append(tuple(round(named[a][i] * (1 - f) + named[b][i] * f) for i in range(3)))
    flat = [c for col in colours for c in col]
    palette_img = Image.new("P", (1, 1))
    palette_img.putpalette(flat + flat[:3] * (256 - len(colours)))
    out = [im.quantize(palette=palette_img, dither=0) for im in imgs]
    out[0].save(path, save_all=True, append_images=out[1:], duration=ms, loop=0, optimize=True, disposal=1)
    return len(out)


def check(ref_path):
    size = 600
    big = Image.new("RGB", (size * 4, size * 4), WHITE)
    draw_gen(ImageDraw.Draw(big), size * 2, size * 2, size * 4)
    mine = big.resize((size, size), Image.LANCZOS)
    ref = Image.open(ref_path).convert("RGBA")
    bg = Image.new("RGBA", ref.size, WHITE)
    ref = Image.alpha_composite(bg, ref).convert("RGB").resize((size, size), Image.LANCZOS)
    diff = ImageChops.difference(mine, ref).convert("L")
    hist = diff.histogram()
    total = size * size
    over = sum(hist[33:]) / total  # pixels differing by more than ~13 %
    mean = sum(i * n for i, n in enumerate(hist)) / total
    print(f"check vs {ref_path.rsplit('/', 1)[-1]}: mean abs diff {mean:.2f}/255, pixels >13% off {100 * over:.2f}%")
    return over


if __name__ == "__main__":
    out = sys.argv[1]
    if "--check" in sys.argv:
        over = check(sys.argv[sys.argv.index("--check") + 1])
        if over > 0.02:
            sys.exit("GEN geometry does not match the official PNG")
    n = render_gif(out)
    print(f"wrote {out}: {n} frames")
