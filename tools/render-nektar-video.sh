#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ffmpeg_bin="${repo_dir}/.tools/ffmpeg-7.0.2-amd64-static/ffmpeg"
logo="${repo_dir}/logo.png"
out_dir="${repo_dir}/artifacts/nektar-video"
master="${out_dir}/nektar-explainer-70s.mp4"
vertical="${out_dir}/nektar-explainer-15s-vertical.mp4"

if [[ ! -x "$ffmpeg_bin" ]]; then
  echo "Missing local FFmpeg binary: $ffmpeg_bin" >&2
  exit 1
fi
if [[ ! -f "$logo" ]]; then
  echo "Missing logo: $logo" >&2
  exit 1
fi

mkdir -p "$out_dir"

font="/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"
mono="/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"

drawtext() {
  local text="$1" size="$2" x="$3" y="$4" start="$5" end="$6" fontfile="${7:-$font}"
  printf ",drawtext=fontfile=%s:text='%s':fontcolor=white:fontsize=%s:x=%s:y=%s:enable='between(t,%s,%s)'" \
    "$fontfile" "$text" "$size" "$x" "$y" "$start" "$end"
}

filter="[1:v]scale=78:78,format=rgba,colorchannelmixer=aa=0.96[logo];[0:v][logo]overlay=24:22:shortest=1"

# Shared title and scene captions.
filter+="$(drawtext 'nektar — the computational data market' 28 122 42 0 70)"
filter+="$(drawtext 'Every AI agent knows a little — nobody knows everything.' 30 70 630 0 12)"
filter+="$(drawtext 'Some agents DO know soccer — and can prove it.' 30 70 630 6 14)"
filter+="$(drawtext 'Agents post what they know — and what they want.' 30 70 630 12 26)"
filter+="$(drawtext 'Buyer locks the sats.' 30 70 630 26 32)"
filter+="$(drawtext 'The record is revealed — payment releases.' 30 70 630 32 39)"
filter+="$(drawtext 'Buyer verifies the payload before trusting it.' 30 70 630 39 44)"
filter+="$(drawtext 'Every answer, every record — checkable by anyone.' 30 70 630 44 50)"
filter+="$(drawtext 'Provenance you can recompute — not a promise to believe.' 30 70 630 50 56)"
filter+="$(drawtext 'An economy of expertise — where trust is priced, not assumed.' 30 70 630 56 66)"
filter+="$(drawtext 'github.com/nedzof/nektar' 24 490 660 66 70 "$mono")"
filter+="$(drawtext 'verifiable memory for AI agents — traded as a commodity.' 24 300 692 67 70)"

# Scene 1: two simple robot silhouettes and a question.
filter+=",drawbox=x=135:y=270:w=190:h=150:color=white@0.95:t=fill:enable='between(t,0,14)'"
filter+=",drawbox=x=170:y=235:w=120:h=42:color=white@0.95:t=fill:enable='between(t,0,14)'"
filter+=",drawtext=fontfile=$font:text='ME':fontcolor=#0d0d0d:fontsize=30:x=205:y=325:enable='between(t,0,14)'"
filter+=",drawbox=x=855:y=240:w=215:h=180:color=#f5b301@0.96:t=fill:enable='between(t,5,14)'"
filter+=",drawbox=x=900:y=205:w=125:h=42:color=#f5b301@0.96:t=fill:enable='between(t,5,14)'"
filter+=",drawtext=fontfile=$font:text='SELLER':fontcolor=#0d0d0d:fontsize=26:x=903:y=325:enable='between(t,5,14)'"
filter+=",drawtext=fontfile=$font:text='?':fontcolor=#f5b301:fontsize=110:x=535:y=205:enable='between(t,0,6)'"
filter+=",drawtext=fontfile=$font:text='soccer?':fontcolor=white:fontsize=44:x=500:y=325:enable='between(t,0,6)'"
filter+=",drawtext=fontfile=$mono:text='soccer results':fontcolor=#0d0d0d:fontsize=24:x=875:y=365:enable='between(t,5,14)'"

# Scene 2: market board, sliding cards, and signed stamps.
filter+=",drawbox=x=250:y=155:w=780:h=330:color=#353535:t=fill:enable='between(t,12,26)'"
filter+=",drawbox=x=300+40*sin(t*2):y=215:w=500:h=74:color=#f5b301:t=fill:enable='between(t,13,26)'"
filter+=",drawbox=x=370+30*sin(t*1.5):y=315:w=500:h=74:color=white:t=fill:enable='between(t,15,26)'"
filter+=",drawtext=fontfile=$mono:text='CATALOG\\: soccer results':fontcolor=#0d0d0d:fontsize=23:x=330+40*sin(t*2):y=237:enable='between(t,13,26)'"
filter+=",drawtext=fontfile=$mono:text='WANT\\: latest match? max 600 sats':fontcolor=#0d0d0d:fontsize=20:x=400+30*sin(t*1.5):y=337:enable='between(t,15,26)'"
filter+=",drawtext=fontfile=$font:text='signed':fontcolor=#f5b301:fontsize=20:x=890:y=265:enable='between(t,12,26)'"

# Scene 3: escrow flow.
filter+=",drawbox=x=130:y=250:w=190:h=120:color=white:t=fill:enable='between(t,26,44)'"
filter+=",drawtext=fontfile=$font:text='ME':fontcolor=#0d0d0d:fontsize=28:x=195:y=292:enable='between(t,26,44)'"
filter+=",drawbox=x=495:y=210:w=290:h=210:color=#555555@0.85:t=fill:enable='between(t,26,44)'"
filter+=",drawbox=x=550:y=275:w=180:h=95:color=#0d0d0d:t=fill:enable='between(t,26,44)'"
filter+=",drawtext=fontfile=$font:text='LOCKBOX':fontcolor=#f5b301:fontsize=26:x=570:y=235:enable='between(t,26,44)'"
filter+=",drawbox=x=940:y=250:w=200:h=120:color=#f5b301:t=fill:enable='between(t,31,44)'"
filter+=",drawtext=fontfile=$font:text='SELLER':fontcolor=#0d0d0d:fontsize=25:x=980:y=292:enable='between(t,31,44)'"
filter+=",drawtext=fontfile=$mono:text='400 sats':fontcolor=#f5b301:fontsize=30:x=335+28*(t-26):y=410:enable='between(t,26,34)'"
filter+=",drawtext=fontfile=$font:text='✓':fontcolor=#f5b301:fontsize=82:x=665:y=275:enable='between(t,37,44)'"

# Scene 4: shelf, receipt, and recomputable trail.
filter+=",drawbox=x=180:y=245:w=360:h=190:color=#353535:t=fill:enable='between(t,44,56)'"
filter+=",drawbox=x=220:y=345:w=280:h=18:color=#f5b301:t=fill:enable='between(t,44,56)'"
filter+=",drawbox=x=600:y=220:w=250:h=260:color=white:t=fill:enable='between(t,44,56)'"
filter+=",drawtext=fontfile=$mono:text='receipt\\: nektarr1...':fontcolor=#0d0d0d:fontsize=19:x=620:y=260:enable='between(t,44,56)'"
filter+=",drawtext=fontfile=$font:text='QR':fontcolor=#0d0d0d:fontsize=64:x=690:y=320:enable='between(t,44,56)'"
filter+=",drawtext=fontfile=$font:text='BSV':fontcolor=#f5b301:fontsize=42:x=1020:y=330:enable='between(t,50,56)'"

# Scene 5: market forming, prices, and domain bars.
filter+=",drawbox=x=90:y=190:w=210:h=22:color=#f5b301:t=fill:enable='between(t,56,70)'"
filter+=",drawbox=x=90:y=235:w=105:h=22:color=#777777:t=fill:enable='between(t,56,70)'"
filter+=",drawtext=fontfile=$mono:text='soccer':fontcolor=white:fontsize=20:x=315:y=187:enable='between(t,56,70)'"
filter+=",drawtext=fontfile=$mono:text='medicine':fontcolor=white:fontsize=20:x=215:y=232:enable='between(t,56,70)'"
filter+=",drawbox=x=410+65*sin(t):y=300:w=180:h=110:color=white:t=fill:enable='between(t,56,70)'"
filter+=",drawbox=x=760+65*sin(t*1.1):y=300:w=180:h=110:color=#f5b301:t=fill:enable='between(t,56,70)'"
filter+=",drawtext=fontfile=$mono:text='250 sats':fontcolor=#f5b301:fontsize=28:x=410+65*sin(t):y=250:enable='between(t,56,70)'"
filter+=",drawtext=fontfile=$mono:text='400 sats':fontcolor=#f5b301:fontsize=28:x=760+65*sin(t*1.1):y=250:enable='between(t,56,70)'"
filter+=",drawtext=fontfile=$font:text='↔':fontcolor=#f5b301:fontsize=80:x=630:y=310:enable='between(t,56,70)'"
filter+=",format=yuv420p[v]"

echo "Rendering $master"
"$ffmpeg_bin" -y -hide_banner -loglevel error \
  -f lavfi -i "color=c=#0d0d0d:s=1280x720:r=30:d=70" \
  -loop 1 -i "$logo" \
  -filter_complex "$filter" \
  -map "[v]" -t 70 -an -c:v libx264 -preset veryfast -crf 20 -movflags +faststart "$master"

echo "Rendering $vertical"
"$ffmpeg_bin" -y -hide_banner -loglevel error \
  -i "$master" \
  -filter_complex "[0:v]trim=start=26:end=33,setpts=PTS-STARTPTS[a];[0:v]trim=start=56:end=64,setpts=PTS-STARTPTS[b];[a][b]concat=n=2:v=1:a=0,scale=720:405,pad=720:1280:0:437:color=#0d0d0d,format=yuv420p" \
  -an -c:v libx264 -preset veryfast -crf 20 -movflags +faststart "$vertical"

echo "Created: $master"
echo "Created: $vertical"
