# OpusRef web frontend

This directory contains the Vue 3 browser client for `opusrefweb`. The Go
companion serves the committed `dist` directory. Production deployment does not
need Node.js.

## Development

Use Node.js 22. Install the exact dependencies and run the checks:

```text
npm ci
npm run typecheck
npm test
npm run build
npm run check:dist
npm run test:e2e
```

The browser never sends or receives an audio container. Each ORWB binary message
contains one complete raw Opus packet. The browser does all Opus encoding and
decoding in a dedicated WebCodecs worker. The Go service does not use a codec.

Every JSON `channel_id` is a canonical unsigned decimal string. The browser
validates the string before it converts the value to `BigInt`. This rule preserves
all 64 bits. The binary ORWB header stores the same value as a network-order
unsigned 64-bit integer.

```mermaid
flowchart LR
    MIC[Microphone] --> AW[Bounded AudioWorklet capture]
    AW --> WK[Dedicated WebCodecs worker]
    WK --> OPUS[One raw Opus packet]
    OPUS --> WSS[One WSS application message]
    WSS --> JB[Bounded timestamp jitter queue]
    JB --> PLAY[Bounded AudioWorklet playout]
```

The worker waits for 60 ms of contiguous decoded PCM. It unwraps the 48 kHz
timestamp and paces output from that clock. It stores no more than 500 ms or 1 MiB
of decoded data. It drops expired PCM. It inserts at most 120 ms of silence for a
positive gap. It resets after a timestamp jump greater than two seconds. A seek or
discontinuity clears decoder, jitter, and playout state.

The capture port has four credits. The encoder queue has four entries. The browser
stops PTT when a capture, encoder, or WebSocket queue reaches its limit. The playout
worklet uses a fixed 500 ms ring buffer. It does not use an unbounded media array.

Audio requires raw Opus support, a 48 kHz `AudioContext`, `AudioWorklet`, and
WebCodecs audio encoder and decoder support. The UI keeps monitoring and account
functions available when the capability test fails.

The worker tests the complete encoder configuration before it starts audio. A
Chromium browser test runs the production worker. It encodes three packets, checks
sequence and timestamp values, checks the payload limits, and decodes the packets.
The test does not qualify a release on Safari or an Android device.

The browser closes privileged media when the page becomes hidden. It also closes
privileged media for every `pagehide` event. If a session becomes invalid in open
access mode, the browser stops PTT and playback. It keeps anonymous live listening
available. If the WebSocket closes, the UI requires the user to select Listen live
again. It does not restart audio without a user action.

Playwright uses loopback servers to test control messages, ORWB media, playback,
close handling, anonymous downgrade, and the production bundle over HTTPS and
same-origin WSS. The local HTTPS certificate is test-only. The system release gate
must also test the configured reverse proxy and the devices in
[QUALIFICATION.md](QUALIFICATION.md).

## Production assets

Run `npm run build`. Commit the resulting `dist` directory with the source and
lock file. CI rebuilds the directory and checks for a clean Git worktree.

Playwright WebKit is a regression target. It does not qualify Safari support.
Release qualification must test real Safari 26 on macOS and iOS. It must also
test Chrome on Android.
