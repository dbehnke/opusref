# Opus packet fixtures

These files contain one lowercase hexadecimal Opus packet on each line. Tests
convert the text to packet bytes. The reflector does not parse the packet.

The source is mono PCM at 48 kHz. The generator uses a fixed amplitude and a
fixed phase. Silence and DTX use zero PCM. Tone uses 1 kHz. Sweep uses a linear
300 Hz through 3 kHz source. The FEC case enables in-band FEC and 10 percent
expected packet loss.

Regeneration is optional. It requires `ffmpeg` with libopus. Generate the PCM
source with a fixed lavfi expression. Encode one frame with `-c:a libopus`,
`-application voip`, and the duration in the file name. Extract the first Opus
packet and write its lowercase hexadecimal value. For FEC, also use
`-fec 1 -packet_loss 10`. Update `SHA256SUMS` after regeneration.

Normal tests do not link libopus. Endpoint-only verification can decode these
fixtures. Do not compare decoded PCM bytes because Opus is lossy.
