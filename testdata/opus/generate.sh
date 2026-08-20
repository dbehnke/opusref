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
ffmpeg -y -loglevel error -f lavfi -i 'sine=frequency=700:sample_rate=16000:duration=0.20' -ar 16000 -ac 1 -c:a libopus -application voip -frame_duration 20 -vbr off -b:a 12k -fec 1 -packet_loss 30 "$fixture_tmp/fec-20ms.ogg"

for source in "$fixture_tmp"/*.ogg; do
	case "$source" in */fec-20ms.ogg) continue ;; esac
    target=$(basename "$source" .ogg).hex
    ffprobe -v error -show_entries packet=data -show_data -of default=nw=1:nk=1 "$source" |
        awk 'BEGIN { found=0 } /^00000000:/ { found=1 } found && /^0000/ { line=$0; sub(/^[^:]*: /,"",line); sub(/  .*/,"",line); gsub(/ /,"",line); printf "%s",line; next } found { print ""; exit }' >"$target"
done

extract_packet() {
	ffprobe -v error -show_entries packet=data -show_data -of default=nw=1:nk=1 "$1" |
		awk -v wanted="$2" '/^00000000:/ { packet++; if (packet>wanted) { print ""; exit }; copying=(packet==wanted) } copying && /^0000/ { line=$0; sub(/^[^:]*: /,"",line); sub(/  .*/,"",line); gsub(/ /,"",line); printf "%s",line }' >"$3"
}
extract_packet "$fixture_tmp/fec-20ms.ogg" 4 fec-context-20ms.hex
extract_packet "$fixture_tmp/fec-20ms.ogg" 5 fec-prior-20ms.hex
extract_packet "$fixture_tmp/fec-20ms.ogg" 6 fec-recovery-20ms.hex

opus_cflags=$(pkg-config --cflags opus 2>/dev/null || printf '%s' '-I/opt/homebrew/include/opus')
opus_libs=$(pkg-config --libs opus 2>/dev/null || printf '%s' '-L/opt/homebrew/lib -lopus')
# shellcheck disable=SC2086
cc -std=c11 -Wall -Wextra -Werror $opus_cflags verify_fec.c $opus_libs -o "$fixture_tmp/verify_fec"
"$fixture_tmp/verify_fec" fec-context-20ms.hex fec-prior-20ms.hex fec-recovery-20ms.hex

shasum -a 256 ./*.hex | sed 's#  \./#  #' >SHA256SUMS
