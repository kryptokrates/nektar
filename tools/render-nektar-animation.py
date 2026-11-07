from __future__ import annotations

import math
import shutil
import subprocess
from pathlib import Path

from PIL import Image, ImageDraw, ImageFilter, ImageFont


ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "artifacts" / "nektar-video"
FRAMES = OUT / "frames"
FFMPEG = ROOT / ".tools" / "ffmpeg-7.0.2-amd64-static" / "ffmpeg"
LOGO = ROOT / "logo.png"
W, H, FPS = 640, 360, 24
BG = (11, 13, 16)
PANEL = (22, 27, 32)
PANEL_2 = (29, 35, 41)
AMBER = (245, 179, 1)
AMBER_SOFT = (255, 214, 104)
WHITE = (245, 246, 242)
MUTED = (145, 154, 164)


def fnt(size: int, mono=False):
    name = "DejaVuSansMono.ttf" if mono else "DejaVuSans.ttf"
    return ImageFont.truetype(f"/usr/share/fonts/truetype/dejavu/{name}", size)


def ease(x: float) -> float:
    x = max(0.0, min(1.0, x))
    return x * x * (3 - 2 * x)


def clamp(x, lo=0.0, hi=1.0):
    return max(lo, min(hi, x))


def text(draw, xy, value, size=16, fill=WHITE, mono=False, anchor=None):
    draw.text(xy, value, font=fnt(size, mono), fill=fill, anchor=anchor)


def rounded(draw, xy, fill, outline=None, width=1, radius=8):
    draw.rounded_rectangle(xy, radius=radius, fill=fill, outline=outline, width=width)


def glow(image, xy, color=AMBER, radius=18, strength=0.55):
    layer = Image.new("RGBA", image.size, (0, 0, 0, 0))
    d = ImageDraw.Draw(layer)
    x0, y0, x1, y1 = xy
    d.ellipse((x0 - radius, y0 - radius, x1 + radius, y1 + radius), fill=(*color, int(180 * strength)))
    image.alpha_composite(layer.filter(ImageFilter.GaussianBlur(radius)))


def stamp(draw, x, y, size=18, pulse=1.0):
    r = size * (0.86 + 0.14 * pulse)
    draw.ellipse((x - r, y - r, x + r, y + r), fill=AMBER)
    text(draw, (x, y + 1), "✓", max(8, int(size * 1.05)), BG, anchor="mm")


def robot(image, x, y, label, accent=WHITE, scale=1.0, bob=0.0):
    d = ImageDraw.Draw(image)
    y += int(math.sin(bob) * 3 * scale)
    bw, bh = int(96 * scale), int(70 * scale)
    d.ellipse((x + 8, y + bh + 8, x + bw - 8, y + bh + 16), fill=(0, 0, 0, 100))
    rounded(d, (x, y, x + bw, y + bh), accent, radius=int(12 * scale))
    rounded(d, (x + 15 * scale, y - 22 * scale, x + bw - 15 * scale, y + 8 * scale), accent, radius=int(9 * scale))
    d.ellipse((x + 31 * scale, y - 13 * scale, x + 41 * scale, y - 3 * scale), fill=BG)
    d.ellipse((x + 55 * scale, y - 13 * scale, x + 65 * scale, y - 3 * scale), fill=BG)
    d.line((x + bw / 2 - 10 * scale, y + 33 * scale, x + bw / 2 + 10 * scale, y + 33 * scale), fill=BG, width=max(1, int(2 * scale)))
    text(d, (x + bw / 2, y + bh / 2 + 11 * scale), label, max(9, int(12 * scale)), BG, anchor="mm")


def card(image, x, y, w, h, label, fill=WHITE, signed=True, slide=0.0, pulse=1.0):
    d = ImageDraw.Draw(image)
    x += int(slide)
    rounded(d, (x + 3, y + 5, x + w + 3, y + h + 5), (0, 0, 0, 100), radius=7)
    rounded(d, (x, y, x + w, y + h), fill, radius=7)
    text(d, (x + 13, y + h / 2), label, 11, BG, mono=True, anchor="lm")
    if signed:
        stamp(d, x + w - 17, y + h / 2, 10, pulse)


def background(image, t):
    d = ImageDraw.Draw(image)
    for x in range(0, W, 40):
        d.line((x, 0, x, H), fill=(17, 21, 25), width=1)
    for y in range(0, H, 40):
        d.line((0, y, W, y), fill=(17, 21, 25), width=1)
    d.line((0, 300, W, 300), fill=(40, 45, 50), width=1)
    text(d, (24, 20), "nektar", 20, WHITE)
    text(d, (103, 22), "THE COMPUTATIONAL DATA MARKET", 8, MUTED, mono=True)
    d.line((24, 51, 616, 51), fill=AMBER, width=2)
    d.line((24, 351, 24 + (t / 80.0) * 592, 351), fill=AMBER, width=2)


def caption(image, label, value, scene_no):
    d = ImageDraw.Draw(image)
    rounded(d, (24, 310, 616, 342), PANEL, outline=(55, 63, 70), width=1, radius=5)
    text(d, (38, 326), f"0{scene_no}", 10, AMBER, mono=True, anchor="lm")
    text(d, (69, 326), label.upper(), 9, MUTED, mono=True, anchor="lm")
    text(d, (182, 326), value, 12, WHITE, anchor="lm")


def scene_1(image, u, logo):
    d = ImageDraw.Draw(image)
    p = ease(clamp(u))
    robot(image, int(50 + 30 * (1 - p)), 190, "ME", WHITE, bob=u * 5)
    robot(image, int(470 - 28 * (1 - p)), 185, "SELLER", AMBER_SOFT, bob=u * 5 + 1)
    text(d, (294, 115), "?", 72, AMBER, anchor="mm")
    text(d, (294, 171), "soccer?", 25, WHITE, anchor="mm")
    card(image, 455, 235, 130, 38, "soccer results", AMBER_SOFT, slide=12 * math.sin(u * math.pi), pulse=0.5 + 0.5 * math.sin(u * 8))
    stamp(d, 600, 190, 15, 0.5 + 0.5 * math.sin(u * 9))
    caption(image, "THE PROBLEM", "Every agent knows a little. Every gap is an opportunity.", 1)


def scene_2(image, u, logo):
    d = ImageDraw.Draw(image)
    rounded(d, (125, 75, 515, 278), PANEL_2, outline=(84, 96, 108), width=2, radius=7)
    text(d, (145, 96), "LIVE MARKET", 10, AMBER, mono=True)
    cards = [(118, AMBER_SOFT, "CATALOG: soccer results"), (165, WHITE, "WANT: latest match? max 600 sats"), (212, WHITE, "CATALOG: weather records")]
    for i, (y, fill, label) in enumerate(cards):
        slide = (1 - ease(clamp((u - i * 0.16) / 0.5))) * 65
        card(image, 158, y, 300, 32, label, fill, slide=slide, pulse=0.5 + 0.5 * math.sin(u * 10 + i))
    robot(image, 35, 190, "ME", WHITE, bob=u * 5)
    robot(image, 520, 188, "SELLER", AMBER_SOFT, bob=u * 5 + 1)
    d.line((132, 208, 508, 208), fill=AMBER, width=2)
    d.polygon([(508, 208), (497, 203), (497, 213)], fill=AMBER)
    caption(image, "DISCOVERY", "A public board of signed cards. Readable. Verifiable.", 2)


def scene_3(image, u, logo):
    d = ImageDraw.Draw(image)
    robot(image, 34, 185, "ME", WHITE, bob=u * 5)
    rounded(d, (205, 140, 435, 256), PANEL_2, outline=AMBER, width=2, radius=10)
    text(d, (320, 158), "ESCROW", 12, AMBER, mono=True, anchor="mm")
    d.ellipse((294, 190, 346, 242), outline=WHITE, width=3)
    text(d, (320, 215), "$", 24, AMBER, anchor="mm")
    robot(image, 510, 185, "SELLER", AMBER_SOFT, bob=u * 5 + 1)
    px = int(125 + 390 * ease(clamp(u * 1.2)))
    for i in range(5):
        x = px - i * 13
        if x > 105:
            glow(image, (x - 4, 279, x + 4, 287), AMBER, 10, 0.3)
            d.ellipse((x - 4, 279, x + 4, 287), fill=AMBER)
    d.line((112, 283, px, 283), fill=AMBER, width=3)
    text(d, (320, 286), "400 sats", 12, AMBER, mono=True, anchor="mm")
    if u > 0.62:
        pulse = 1 + 0.15 * math.sin((u - 0.62) * 20)
        glow(image, (320 - 16 * pulse, 203 - 16 * pulse, 320 + 16 * pulse, 235 + 16 * pulse), AMBER, 22, 0.28)
        text(d, (320, 218), "✓", 39, AMBER, anchor="mm")
        mark = logo.resize((30, 30), Image.Resampling.LANCZOS)
        image.alpha_composite(mark, (305, 176))
    caption(image, "THE DEAL", "Payment locks. The record reveals. The money moves.", 3)


def scene_4(image, u, logo):
    d = ImageDraw.Draw(image)
    rounded(d, (50, 130, 280, 265), PANEL_2, outline=(82, 92, 101), width=2, radius=6)
    for y in (220, 242):
        d.line((50, y, 280, y), fill=AMBER, width=3)
    for x, h in ((85, 65), (135, 85), (185, 52), (235, 76)):
        d.rectangle((x, 220 - h, x + 25, 220), fill=(63, 73, 84))
    card(image, 106, 156, 118, 48, "soccer file", WHITE, signed=True, slide=-15 * (1 - ease(u)))
    rounded(d, (340, 115, 522, 267), WHITE, radius=5)
    text(d, (356, 140), "receipt:", 13, BG, mono=True)
    text(d, (356, 160), "nektarr1...", 13, BG, mono=True)
    text(d, (430, 220), "QR", 34, BG, anchor="mm")
    text(d, (570, 188), "BSV", 22, AMBER, mono=True, anchor="mm")
    trail = int(470 + 75 * ease(clamp(u * 1.5)))
    d.line((522, 188, trail, 188), fill=AMBER, width=2)
    d.polygon([(trail, 188), (trail - 8, 183), (trail - 8, 193)], fill=AMBER)
    caption(image, "PROOF, NOT PROMISES", "Every answer comes with a receipt. Checkable by anyone.", 4)


def scene_5(image, u, logo):
    d = ImageDraw.Draw(image)
    text(d, (32, 80), "REPUTATION BY DOMAIN", 10, MUTED, mono=True)
    text(d, (32, 103), "soccer", 10, WHITE, mono=True)
    rounded(d, (90, 97, 220, 111), (49, 58, 66), radius=7)
    rounded(d, (90, 97, 196, 111), AMBER, radius=7)
    text(d, (32, 133), "medicine", 10, WHITE, mono=True)
    rounded(d, (90, 127, 220, 141), (49, 58, 66), radius=7)
    rounded(d, (90, 127, 130, 141), (75, 75, 75), radius=7)
    for i, x in enumerate((230, 332, 434, 536)):
        robot(image, x - 40 + int(math.sin(u * 4 + i) * 5), 190 + (i % 2) * 12, "AGENT", AMBER_SOFT if i % 2 else WHITE, 0.68, u * 5 + i)
    card(image, 215, 151, 86, 27, "250 sats", AMBER_SOFT, signed=False, slide=10 * math.sin(u * 4))
    card(image, 360, 151, 86, 27, "400 sats", AMBER_SOFT, signed=False, slide=-10 * math.sin(u * 4))
    for i in range(6):
        x = 180 + ((u * 110 + i * 82) % 380)
        y = 170 + (i % 3) * 21
        d.line((x, y, x + 20, y), fill=AMBER, width=2)
        d.polygon([(x + 20, y), (x + 14, y - 4), (x + 14, y + 4)], fill=AMBER)
    caption(image, "THE MARKET FORMS", "Prices clear. Reputations build. The market opens.", 5)
    if u > 0.72:
        text(d, (320, 286), "github.com/nedzof/nektar", 13, WHITE, mono=True, anchor="mm")


def draw_frame(t: float, logo):
    base = Image.new("RGBA", (W, H), (*BG, 255))
    background(base, t)
    base.alpha_composite(logo, (570, 6))
    scene_defs = [(0, 14, scene_1), (14, 30, scene_2), (30, 50, scene_3), (50, 64, scene_4), (64, 80, scene_5)]
    current = next((item for item in scene_defs if item[0] <= t < item[1]), scene_defs[-1])
    start, end, fn = current
    fn(base, clamp((t - start) / (end - start)), logo)
    transition = 0.55
    if t - start < transition and start > 0:
        previous = scene_defs[scene_defs.index(current) - 1]
        prev_layer = Image.new("RGBA", (W, H), (*BG, 255))
        background(prev_layer, t)
        previous[2](prev_layer, 1.0, logo)
        base = Image.blend(prev_layer, base, ease((t - start) / transition))
    if end - t < transition and end < 80:
        next_scene = scene_defs[scene_defs.index(current) + 1]
        next_layer = Image.new("RGBA", (W, H), (*BG, 255))
        background(next_layer, t)
        next_scene[2](next_layer, 0.0, logo)
        base = Image.blend(base, next_layer, ease((t - (end - transition)) / transition))
    return base.convert("RGB")


def encode(frame_dir: Path, output: Path, duration: float):
    subprocess.run([
        str(FFMPEG), "-y", "-hide_banner", "-loglevel", "error",
        "-framerate", str(FPS), "-i", str(frame_dir / "%06d.png"),
        "-t", str(duration), "-vf", "scale=1280:720:flags=lanczos,format=yuv420p",
        "-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-movflags", "+faststart", str(output)
    ], check=True)


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    if FRAMES.exists():
        shutil.rmtree(FRAMES)
    FRAMES.mkdir()
    logo = Image.open(LOGO).convert("RGBA").resize((42, 42), Image.Resampling.LANCZOS)
    for i in range(80 * FPS):
        frame = draw_frame(i / FPS, logo)
        frame.save(FRAMES / f"{i:06d}.png", optimize=True)
    master = OUT / "nektar-explainer-80s.mp4"
    encode(FRAMES, master, 80)
    vertical = OUT / "nektar-explainer-15s-vertical.mp4"
    subprocess.run([
        str(FFMPEG), "-y", "-hide_banner", "-loglevel", "error", "-i", str(master),
        "-filter_complex", "[0:v]trim=start=30:end=37.5,setpts=PTS-STARTPTS[a];[0:v]trim=start=64:end=71.5,setpts=PTS-STARTPTS[b];[a][b]concat=n=2:v=1:a=0,scale=720:405,pad=720:1280:0:437:color=#0b0d10,format=yuv420p",
        "-c:v", "libx264", "-preset", "veryfast", "-crf", "18", "-movflags", "+faststart", str(vertical)
    ], check=True)
    print(master)
    print(vertical)


if __name__ == "__main__":
    main()
