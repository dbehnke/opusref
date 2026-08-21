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
npm run test:e2e
```

The browser never sends or receives an audio container. Each ORWB binary message
contains one complete raw Opus packet. The browser does all Opus encoding and
decoding in a dedicated WebCodecs worker. The Go service does not use a codec.

Audio requires raw Opus support, a 48 kHz `AudioContext`, `AudioWorklet`, and
WebCodecs audio encoder and decoder support. The UI keeps monitoring and account
functions available when the capability test fails.

## Production assets

Run `npm run build`. Commit the resulting `dist` directory with the source and
lock file. CI rebuilds the directory and checks for a clean Git worktree.

Playwright WebKit is a regression target. It does not qualify Safari support.
Release qualification must test real Safari 26 on macOS and iOS. It must also
test Chrome on Android.
