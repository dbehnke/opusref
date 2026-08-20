# Opus packet fixtures

These files contain one lowercase hexadecimal Opus packet on each line. Tests
convert the text to packet bytes. The reflector does not parse the packet.

The source is mono PCM at 48 kHz. FFmpeg 9.0.1 and libopus 1.6.1 generated this
set. The encoder uses the `voip` application, a 16 kbit/s rate, and one channel.
Silence and DTX use zero PCM. Tone uses 1 kHz. Sweep uses the expression
`0.2*sin(2*PI*(300*t+33750*t*t))` for a 300 Hz through 3 kHz sweep. The FEC files
are three consecutive 20 ms packets from one 16 kHz, 700 Hz SILK stream. The
fixture encoder uses 30 percent expected loss and a 12 kbit/s rate to select
in-band FEC. A 48 kHz Opus decoder still produces 960 samples for each packet.
`fec-context-20ms` establishes decoder state. `fec-recovery-20ms` contains
recovery data for the intentionally omitted `fec-prior-20ms` packet.

Regeneration is optional. Run `sh generate.sh` in this directory. The script
requires the recorded FFmpeg and libopus versions, a C compiler, and libopus
headers. It extracts three consecutive FEC packets. It then simulates loss of
the prior packet. It requires libopus to decode 960 non-silent recovery samples
from the next packet with `decode_fec=1`. The script updates `SHA256SUMS`.
Review all changed packets before you commit them.

Normal tests do not link libopus. Endpoint-only verification can decode these
fixtures. Do not compare decoded PCM bytes because Opus is lossy.
