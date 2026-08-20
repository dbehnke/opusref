# OpusRef Agent Contract

## Current maturity

OpusRef is a protocol and architecture bootstrap. The Go packages and commands
compile, but networking is not implemented. Do not describe the reflector or
client as functional until implementation and interoperability tests prove it.

## Normative documents

- `docs/protocol-v1.md` defines the wire protocol.
- `docs/architecture.md` defines server and client boundaries.
- `docs/monitoring.md` defines monitoring behavior and metric names.

When code and a normative document disagree, stop and resolve the discrepancy.
Do not silently change the protocol in code.

## Engineering rules

1. The reflector moves opaque media. It must never encode, decode, transcode,
   record, or inspect Opus payloads.
2. Keep the single-channel, first-talker-wins, half-duplex model unless the
   operator approves a protocol version change.
3. Use TDD for all production behavior. Write a failing test before production
   code, make the smallest change that passes, and then refactor. Wire parsing,
   session state, and floor arbitration require unit, negative, and boundary
   tests.
4. Apply SOLID principles. Keep interfaces small, keep protocol parsing apart
   from transport and policy, inject clocks and network boundaries, and make
   components replaceable in tests.
5. Write user-facing and technical documentation in accordance with
   ASD-STE-100 Simplified Technical English. Use short sentences, one meaning
   per term, active voice, and consistent approved terminology.
6. Use Mermaid for flow, sequence, and state diagrams in Markdown. Do not add
   ASCII-art diagrams when Mermaid can show the same relationship.
7. Reject malformed input without panics, unbounded allocation, or reflection
   amplification.
8. Do not put callsigns, reflector IDs, stream IDs, session IDs, IP addresses,
   or data type IDs in Prometheus labels.
9. Never log or return a shared key or HMAC input containing the key.
10. Update all three normative documents and golden vectors in the same change
   when wire behavior changes.
