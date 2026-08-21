# OpusRef

OpusRef is a digital voice reflector protocol for amateur
radio applications. It moves opaque Opus audio packets and typed data frames
between connected clients on one half-duplex channel. The protocol is for IP
networks only. It is not an over-the-air waveform.

This repository contains the OpusRef v1 specification, a UDP reflector, a raw
frame client, a diagnostic command, the optional web companion, monitoring
handlers, and tests.

`opusrefd` moves opaque media packets. It does not capture, encode, decode,
transcode, play, record, or inspect audio. The Go code in `opusrefweb` also
keeps Opus packets opaque. It can store the packets in the ORAR archive format.
The browser uses WebCodecs to capture, encode, decode, and play audio.

Read these documents before implementation:

- [Wire protocol](docs/protocol-v1.md)
- [Server and client architecture](docs/architecture.md)
- [Monitoring](docs/monitoring.md)
- [Web companion](docs/web-console.md)
- [Release qualification](docs/release-qualification.md)

## Commands

- `opusrefd --config config.yaml` starts the reflector and monitoring server.
- `opusrefctl listen` writes received `ORR1` records to standard output.
- `opusrefctl transmit` reads `ORR1` records from standard input.
- `opusrefweb serve --config opusrefweb.example.yaml` starts the optional web
  companion.
- `opusrefweb admin create --config opusrefweb.example.yaml` creates an
  administrator account.
- `opusrefweb admin recover --config opusrefweb.example.yaml --username NAME`
  recovers an administrator account.

Use `opusrefctl listen --help` or `opusrefctl transmit --help` for flags. Logs
and errors use standard error. Standard output contains diagnostic records only.

## Bootstrap checks

```sh
go test ./...
go vet ./...
go build ./cmd/opusrefd ./cmd/opusrefctl ./cmd/opusrefweb
cd web
npm ci
npm audit --audit-level=high
npm run typecheck
npm test -- --run
npm run build
npm run test:e2e
npm run test:system-proxy
```

Copy `config.example.yaml` to `config.yaml` for the reflector. Copy
`opusrefweb.example.yaml` for the web companion. Local configuration is
ignored by Git.

## License

MIT. See [LICENSE](LICENSE).
