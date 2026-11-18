"""Generate the multi-resolution application icon (icon.ico + icon.png).

Pure Python (no Pillow needed). Shapes are drawn with signed distance
functions, so every size is rendered natively with smooth anti-aliasing
instead of being scaled down from one big bitmap.

Design: two offset chat bubbles, WhatsApp green over Telegram blue, with
tails pointing opposite ways: two conversations in one app.

Usage:  python gen_icon.py
"""

import math
import struct
import zlib

SIZES = [16, 20, 24, 32, 40, 48, 64, 96, 128, 256]

GREEN = (0x21, 0xC0, 0x63)   # WhatsApp half (matches the sidebar)
BLUE = (0x2A, 0xAB, 0xEE)    # Telegram half
def clamp(v, lo=0.0, hi=1.0):
    return lo if v < lo else hi if v > hi else v


def lerp(a, b, t):
    return a + (b - a) * t


def smoothstep(e0, e1, x):
    t = clamp((x - e0) / (e1 - e0))
    return t * t * (3 - 2 * t)


# ---- signed distance functions (negative = inside) -------------------------

def sd_round_box(px, py, cx, cy, hw, hh, r):
    qx = abs(px - cx) - hw + r
    qy = abs(py - cy) - hh + r
    outside = math.hypot(max(qx, 0.0), max(qy, 0.0))
    inside = min(max(qx, qy), 0.0)
    return outside + inside - r


def sd_circle(px, py, cx, cy, r):
    return math.hypot(px - cx, py - cy) - r


def sd_triangle(px, py, pts):
    """Exact SDF of a triangle (Inigo Quilez)."""
    (x0, y0), (x1, y1), (x2, y2) = pts
    e0x, e0y = x1 - x0, y1 - y0
    e1x, e1y = x2 - x1, y2 - y1
    e2x, e2y = x0 - x2, y0 - y2
    v0x, v0y = px - x0, py - y0
    v1x, v1y = px - x1, py - y1
    v2x, v2y = px - x2, py - y2

    def proj(vx, vy, ex, ey):
        t = clamp((vx * ex + vy * ey) / (ex * ex + ey * ey))
        return vx - ex * t, vy - ey * t

    p0 = proj(v0x, v0y, e0x, e0y)
    p1 = proj(v1x, v1y, e1x, e1y)
    p2 = proj(v2x, v2y, e2x, e2y)
    s = 1.0 if e0x * e2y - e0y * e2x > 0 else -1.0
    dist = math.sqrt(min(
        p0[0] ** 2 + p0[1] ** 2,
        p1[0] ** 2 + p1[1] ** 2,
        p2[0] ** 2 + p2[1] ** 2,
    ))
    sign = min(
        s * (v0x * e0y - v0y * e0x),
        s * (v1x * e1y - v1y * e1x),
        s * (v2x * e2y - v2y * e2x),
    )
    return -dist if sign > 0 else dist


def coverage(d, px_size):
    """Analytic anti-aliasing: SDF distance -> pixel coverage."""
    return clamp(0.5 - d / px_size)


def over(dst, color, alpha):
    """Composite a straight-alpha colour over a straight-alpha pixel."""
    if alpha <= 0:
        return dst
    dr, dg, db, da = dst
    out_a = alpha + da * (1 - alpha)
    if out_a <= 0:
        return (0.0, 0.0, 0.0, 0.0)
    k = da * (1 - alpha)
    return (
        (color[0] * alpha + dr * k) / out_a,
        (color[1] * alpha + dg * k) / out_a,
        (color[2] * alpha + db * k) / out_a,
        out_a,
    )


def render(size):
    """Two offset chat bubbles: green (top, tail left) over blue (bottom, tail right)."""
    px = 1.0 / size
    small = size <= 24
    m = 0.04 if small else 0.06          # outer margin
    tail = 0.07                           # room below each bubble for its tail
    gap = 0.06 if small else 0.07
    bh = (1 - 2 * m - gap - tail) / 2    # bubble height
    hw = 0.39 if small else 0.37          # bubble half width
    r = bh * (0.40 if small else 0.42)
    top = (0.5 - (0.5 - m - hw), m + bh / 2)
    bot = (0.5 + (0.5 - m - hw), m + bh + gap + bh / 2)
    top_bottom = top[1] + bh / 2
    bot_bottom = bot[1] + bh / 2
    tl = top[0] - hw
    br = bot[0] + hw
    top_tail = [(tl + 0.10, top_bottom - 0.08), (tl + 0.26, top_bottom - 0.01), (tl + 0.02, top_bottom + tail)]
    bot_tail = [(br - 0.10, bot_bottom - 0.08), (br - 0.26, bot_bottom - 0.01), (br - 0.02, bot_bottom + tail)]

    rows = []
    for y in range(size):
        row = bytearray()
        for x in range(size):
            fx = (x + 0.5) * px
            fy = (y + 0.5) * px
            pix = (0.0, 0.0, 0.0, 0.0)
            d_top = min(sd_round_box(fx, fy, top[0], top[1], hw, bh / 2, r),
                        sd_triangle(fx, fy, top_tail) - 0.012)
            d_bot = min(sd_round_box(fx, fy, bot[0], bot[1], hw, bh / 2, r),
                        sd_triangle(fx, fy, bot_tail) - 0.012)
            pix = over(pix, GREEN, coverage(d_top, px))
            pix = over(pix, BLUE, coverage(d_bot, px))
            rr, g, b, a = pix
            row += bytes((
                int(clamp(rr / 255) * 255 + 0.5),
                int(clamp(g / 255) * 255 + 0.5),
                int(clamp(b / 255) * 255 + 0.5),
                int(clamp(a) * 255 + 0.5),
            ))
        rows.append(bytes(row))
    return rows


def png_bytes(size, rows):
    def chunk(tag, data):
        c = struct.pack(">I", len(data)) + tag + data
        return c + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)

    raw = b"".join(b"\x00" + r for r in rows)
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0))
        + chunk(b"IDAT", zlib.compress(raw, 9))
        + chunk(b"IEND", b"")
    )


def main():
    images = []
    for s in SIZES:
        images.append((s, png_bytes(s, render(s))))
        print(f"rendered {s}x{s}")

    # ICO container with PNG-compressed entries (supported since Windows Vista).
    header = struct.pack("<HHH", 0, 1, len(images))
    offset = 6 + 16 * len(images)
    entries = b""
    blobs = b""
    for s, data in images:
        dim = 0 if s >= 256 else s
        entries += struct.pack("<BBBBHHII", dim, dim, 0, 0, 1, 32, len(data), offset)
        offset += len(data)
        blobs += data

    with open("icon.ico", "wb") as f:
        f.write(header + entries + blobs)
    with open("icon.png", "wb") as f:
        f.write(dict(images)[256])
    print("wrote icon.ico and icon.png")


if __name__ == "__main__":
    main()
