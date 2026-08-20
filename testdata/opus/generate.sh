#!/bin/sh
set -eu

ffmpeg -version | sed -n '1p' | grep 'ffmpeg version 9.0.1' >/dev/null
opusenc --version 2>&1 | grep 'libopus 1.6.1' >/dev/null

fixture_tmp=$(mktemp -d)
trap 'rm -rf "$fixture_tmp"' EXIT HUP INT TERM

ffmpeg -y -loglevel error -f lavfi -i 'anullsrc=r=48000:cl=mono:d=0.02' -c:a libopus -application voip -frame_duration 20 -vbr off -b:a 16k "$fixture_tmp/silence-20ms.ogg"
ffmpeg -y -loglevel error -f lavfi -i 'sine=frequency=1000:sample_rate=48000:duration=0.02' -ac 1 -c:a libopus -application voip -frame_duration 20 -vbr off -b:a 16k "$fixture_tmp/tone-20ms.ogg"
ffmpeg -y -loglevel error -f lavfi -i 'aevalsrc=0.2*sin(2*PI*(300*t+33750*t*t)):s=48000:d=0.04' -ac 1 -c:a libopus -application voip -frame_duration 40 -vbr off -b:a 16k "$fixture_tmp/sweep-40ms.ogg"
ffmpeg -y -loglevel error -f lavfi -i 'anullsrc=r=48000:cl=mono:d=0.04' -c:a libopus -application voip -frame_duration 20 -vbr on -b:a 16k -dtx 1 "$fixture_tmp/dtx-20ms.ogg"
ffmpeg -y -loglevel error -f lavfi -i 'sine=frequency=700:sample_rate=48000:duration=0.02' -ac 1 -c:a libopus -application voip -frame_duration 20 -vbr off -b:a 16k -fec 1 -packet_loss 10 "$fixture_tmp/fec-20ms.ogg"

for source in "$fixture_tmp"/*.ogg; do
    target=$(basename "$source" .ogg).hex
    ffprobe -v error -show_entries packet=data -show_data -of default=nw=1:nk=1 "$source" |
        awk 'BEGIN { found=0 } /^00000000:/ { found=1 } found && /^0000/ { line=$0; sub(/^[^:]*: /,"",line); sub(/  .*/,"",line); gsub(/ /,"",line); printf "%s",line; next } found { print ""; exit }' >"$target"
done

shasum -a 256 ./*.hex | sed 's#  \./#  #' >SHA256SUMS
