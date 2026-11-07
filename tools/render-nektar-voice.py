from __future__ import annotations

import asyncio
import re
import subprocess
from pathlib import Path

import edge_tts


ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "artifacts" / "nektar-video"
FFMPEG = ROOT / ".tools" / "ffmpeg-7.0.2-amd64-static" / "ffmpeg"
VOICE = "en-US-AndrewNeural"
RATE = "-12%"


async def synthesize(text: str, destination: Path):
    for attempt in range(3):
        try:
            communicate = edge_tts.Communicate(text, VOICE, rate=RATE)
            with destination.open("wb") as handle:
                async with asyncio.timeout(30):
                    async for chunk in communicate.stream():
                        if chunk["type"] == "audio":
                            handle.write(chunk["data"])
            return
        except (TimeoutError, asyncio.CancelledError):
            if attempt == 2:
                raise
            await asyncio.sleep(1)


async def main():
    OUT.mkdir(parents=True, exist_ok=True)
    paragraphs = [p.strip() for p in (ROOT / "tools" / "nektar-vo.txt").read_text().split("\n\n") if p.strip()]
    sentences = [sentence.strip() for paragraph in paragraphs for sentence in re.split(r"(?<=[.!?])\s+", paragraph) if sentence.strip()]
    parts = []
    for index, sentence in enumerate(sentences, 1):
        path = OUT / f"vo-sentence-{index:03d}.mp3"
        await synthesize(sentence, path)
        parts.append(path)
    list_file = OUT / "vo-parts.txt"
    list_file.write_text("\n".join(f"file '{path}'" for path in parts) + "\n")
    output = OUT / "nektar-vo.mp3"
    subprocess.run([
        str(FFMPEG), "-y", "-hide_banner", "-loglevel", "error",
        "-f", "concat", "-safe", "0", "-i", str(list_file), "-c", "copy", str(output)
    ], check=True)
    print(output)


if __name__ == "__main__":
    asyncio.run(main())
