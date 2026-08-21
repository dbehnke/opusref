# OpusRef

OpusRef is a digital voice reflector protocol for amateur
radio applications. It moves opaque Opus audio packets and typed data frames
between connected clients on one half-duplex channel. The protocol is for IP
networks only. It is not an over-the-air waveform.

This repository contains the OpusRef v1 specification, a UDP reflector, a raw
frame client, a diagnostic command, monitoring handlers, and tests. It does not
capture, encode, decode, transcode, play, record, or inspect audio.

Read these documents before implementation:

- [Wire protocol](docs/protocol-v1.md)
- [Server and client architecture](docs/architecture.md)
- [Monitoring](docs/monitoring.md)

## Commands

- `opusrefd --config config.yaml` starts the reflector and monitoring server.
- `opusrefctl listen` writes received `ORR1` records to standard output.
- `opusrefctl transmit` reads `ORR1` records from standard input.

Use `opusrefctl listen --help` or `opusrefctl transmit --help` for flags. Logs
and errors use standard error. Standard output contains diagnostic records only.

## Bootstrap checks

```sh
go test ./...
go vet ./...
go build ./cmd/opusrefd ./cmd/opusrefctl
```

Copy `config.example.yaml` to `config.yaml`. Local configuration is ignored by
Git.

## License

MIT. See [LICENSE](LICENSE).
