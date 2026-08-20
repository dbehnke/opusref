# Opus packet fixtures

These files contain one lowercase hexadecimal Opus packet on each line. Tests
convert the text to packet bytes. The reflector does not parse the packet.

The source is mono PCM at 48 kHz. FFmpeg 9.0.1 and libopus 1.6.1 generated this
set. The encoder uses the `voip` application, a 16 kbit/s rate, and one channel.
Silence and DTX use zero PCM. Tone uses 1 kHz. Sweep uses the expression
`0.2*sin(2*PI*(300*t+33750*t*t))` for a 300 Hz through 3 kHz sweep. The FEC case
uses a 700 Hz tone, in-band FEC, and 10 percent expected packet loss.

Regeneration is optional. Run `sh generate.sh` in this directory. The script
requires the recorded FFmpeg and libopus versions. It generates all sources,
extracts the first Opus packet, and updates `SHA256SUMS`. Review all changed
packets before you commit them.

Normal tests do not link libopus. Endpoint-only verification can decode these
fixtures. Do not compare decoded PCM bytes because Opus is lossy.
