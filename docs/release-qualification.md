# OpusRefWeb release qualification

This file separates executable repository gates from external release gates.
Passing CI does not qualify a production release by itself.

## Executable repository gates

- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`
- Go wire and WebSocket decoder fuzz smoke tests
- Frontend type, unit, build, and reproducibility checks
- Playwright checks with Chromium, Firefox, and WebKit

The WebKit Playwright result is not a substitute for Safari on Apple hardware.

## External release gates

The release operator must record the date, build commit, device, operating
system, browser version, reverse proxy, and result for each item below. These
checks are not complete for the current pull request.

- Android Chrome through production HTTPS and WSS
- Windows Chrome or Edge through production HTTPS and WSS
- macOS Safari through production HTTPS and WSS
- iOS Safari through production HTTPS and WSS
- Microphone permission, momentary PTT, latched PTT, live receive, playback,
  passkey enrollment, passkey login, and passkey reauthentication
- Proxy restart and reflector restart during live receive and PTT
- 24-hour retention and hard-quota behavior with production storage

Attach the completed qualification record to the release. Do not mark a release
qualified when an item is absent, failed, or recorded against another commit.
