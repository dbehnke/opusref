# OpusRef web console

This document specifies the first OpusRef web companion implementation. Issue 3
is the product specification. The UDP protocol in `protocol-v1.md` stays
unchanged.

## Process boundary

`opusrefweb` is a separate process. It uses two ordinary OpusRef UDP clients. One
client receives audio. One client requests the floor and sends browser audio.
The process does not encode or decode Opus. The browser uses WebCodecs.

```mermaid
flowchart LR
    Browser[Browser WebCodecs] <-->|HTTPS and WSS| Web[opusrefweb]
    Web -->|Receive client| Reflector[opusrefd UDP]
    Web -->|Transmit client| Reflector
    Web --> DB[(SQLite)]
    Web --> Archive[(ORAR files)]
```

## Security

Put an HTTPS reverse proxy in front of the public listener. Preserve the exact
Origin header for WebSocket upgrades. Do not publish the monitor listener. The
public listener does not serve `/metrics`.

The server stores Argon2id password hashes. It stores hashes of session, CSRF,
and reauthentication tokens. It sends a session token only in the secure
`__Host-opusref_session` cookie. An administrator must create each account.

```mermaid
sequenceDiagram
    participant U as User
    participant B as Browser
    participant W as opusrefweb
    U->>B: Enter username and password
    B->>W: Same-origin login request
    W->>W: Verify Argon2id hash
    W-->>B: Secure cookie and CSRF token
    B->>W: WSS hello with CSRF token
    W-->>B: hello_ok
```

## Media

Each ORWB binary message contains one complete Opus packet. The server accepts
1 through 1,168 payload bytes. The server copies payload bytes without a codec
operation. A PTT channel starts at sequence 0 and timestamp 0. Each browser
packet adds 1 to the sequence and 960 to the timestamp.

The ORAR archive stores the OpusRef sequence, 48 kHz timestamp, arrival offset,
and unchanged payload. A final file has the `.orar` suffix. An open recovery file
has the `.partial` suffix. The archive directory must have mode `0700`. Files
must have mode `0600`.

## Start the service

1. Copy `opusrefweb.example.yaml` to an operator-controlled path.
2. Set the final HTTPS origin and WebAuthn RP ID.
3. Optionally configure `password_blocklist_file` with site-specific additions.
   The service always applies its pinned, embedded SecLists 10k corpus first.
4. Create the storage directory with mode `0700`.
5. Run `opusrefweb auth benchmark --config FILE`.
6. Run `opusrefweb admin create --config FILE --username NAME` on a TTY.
7. Start `opusrefd`.
8. Start `opusrefweb serve --config FILE`.

The service refuses readiness when no enabled administrator exists. Stop the
service with SIGTERM. The process removes readiness before it closes the HTTP
listeners and reflector clients.

The receive client and transmit client reconnect with a bounded exponential
delay. Readiness is false unless both clients have authenticated reflector
sessions. A reconnect does not change the browser API. A reflector revoke ends
the active browser PTT channel.

The WebSocket writer uses separate bounded control, live-audio, and playback
queues. Control output has priority. A full live queue drops its queued media
and announces a discontinuity with a new channel. A full playback queue pauses
playback for an explicit resume or seek. A full control queue closes the socket
with code 4409. One slow browser cannot block the reflector clients.

## Passkey flow

```mermaid
sequenceDiagram
    participant B as Browser
    participant W as opusrefweb
    participant A as Authenticator
    B->>W: Request one-use options
    W-->>B: Ceremony ID and options
    B->>A: Create or get a credential
    A-->>B: Signed response
    B->>W: Ceremony ID and signed response
    W->>W: Verify origin, RP ID, user verification, and counter
    W-->>B: Session or enrollment result
```

## PTT flow

```mermaid
stateDiagram-v2
    [*] --> Idle
    Idle --> Requesting: ptt_start
    Requesting --> Transmitting: reflector grant
    Requesting --> Idle: busy or error
    Transmitting --> Stopping: ptt_stop or safety stop
    Stopping --> Idle: stream end
    Transmitting --> Idle: revoke, timeout, or disconnect
```

## Recording recovery

```mermaid
flowchart TD
    A[First audio packet] --> B[Create database row]
    B --> C[Create and sync partial file]
    C --> D[Append opaque Opus packets]
    D --> E[Commit finalizing intent]
    E --> F[Sync and rename final file]
    F --> I[Sync directory and commit final state]
    G[Process restart] --> H[Validate partial and final files]
    H --> I[Finalize known files]
    H --> J[Quarantine orphan or corrupt files]
```

Each queued archive item includes the reflector session ID and stream ID. The
archive worker accepts an item only for the active stream. `STREAM_END` drains
all earlier media before it finalizes the file. A sequence gap, a synthetic end,
an unknown prefix, or archive backpressure makes the recording `partial`.

Recording IDs are canonical UUID strings. File access stays in the configured
archive directory. Playback indexes at most 4,096 packets and 1 MiB of index
data. The default playback duration limit is 15 minutes. Deletion uses the
database `deleting` state and a `.deleting` file. Startup
completes an interrupted deletion. Retention removes expired recordings first.
Quota-full mode stops new archive writes and keeps retained recordings. The
service stays ready and reports degraded recording state. Retention cleanup is
the only automatic operation that deletes recordings.

## API pagination

`GET /api/v1/recordings` accepts `limit`, `cursor`, `callsign`, `status`,
`from`, and `to`. The limit range is 1 through 200. The cursor is opaque. The
result contains `items` and can contain `next_cursor`. Use
`GET /api/v1/recordings/{id}` for one recording.

Passkey option results contain `ceremony_id` and `publicKey` at the same level.
Passkey reauthentication uses `/api/v1/me/reauth/passkey/options` and
`/api/v1/me/reauth/passkey/verify`. A successful verification returns a one-use
reauthentication token that expires after five minutes.

## Shutdown

```mermaid
sequenceDiagram
    participant O as Operator
    participant W as opusrefweb
    participant R as Reflector
    O->>W: SIGTERM
    W->>W: Remove readiness
    W->>R: End active PTT
    W->>W: Close WSS with restart code
    W->>W: Finalize archive as partial
    W->>R: Close UDP clients
    W->>W: Stop HTTP within five seconds
```
