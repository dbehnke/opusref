# OpusRef

OpusRef is a specification-stage digital voice reflector protocol for amateur
radio applications. It moves opaque Opus audio packets and typed data frames
between connected clients on one half-duplex channel. The protocol is for IP
networks only. It is not an over-the-air waveform.

This repository currently contains the OpusRef v1 specification, architecture,
monitoring design, and compiling Go package skeletons. It does not contain a
working reflector or client. In particular, it does not capture, encode,
decode, transcode, play, record, or inspect audio.

Read these documents before implementation:

- [Wire protocol](docs/protocol-v1.md)
- [Server and client architecture](docs/architecture.md)
- [Monitoring](docs/monitoring.md)

## Commands

- `opusrefd` is reserved for the reflector server.
- `opusrefctl` is reserved for the diagnostic client.

Both commands are placeholders and report that networking is not implemented.

## Bootstrap checks

```sh
go test ./...
go vet ./...
go build ./cmd/opusrefd ./cmd/opusrefctl
```

Copy `config.example.yaml` to `config.yaml` only after server implementation
starts. Local configuration is ignored by Git.

## License

MIT. See [LICENSE](LICENSE).

