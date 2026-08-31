# realtime-kit

A small HTTP sidecar over
[`livekit/server-sdk-go`](https://github.com/livekit/server-sdk-go).

It exists for one reason: the app that needs LiveKit
([pakku](https://pack.hushkey.app)) runs on Deno, and the LiveKit server SDK
worth using is Go. Rather than reimplement token signing and the room API in
TypeScript and own that reimplementation forever, the app calls this service and
this service calls the SDK.

## Shape

**Stateless and multi-tenant.** The service stores no LiveKit credentials of its
own. Every request names the deployment it is acting for — URL, API key, API
secret — so one instance serves every workspace, each on its own LiveKit
account. That is what makes the caller's bring-your-own-key model work.

**Not internet-facing.** Callers pass LiveKit API secrets through it in request
bodies. Run it on loopback or a private network between the app and itself, with
TLS if that network is not already private. Every `/v1` route also requires a
shared bearer token (constant-time compared) — defence in depth, not a
substitute for the network boundary.

**Nothing sensitive is logged.** The access log records method, path, status and
duration. Request bodies (which carry API secrets) and responses (which carry
minted join tokens) never reach a log line, and neither do error messages
assembled from them.

## Running

```sh
make dev
```

`make dev` runs the complete local stack without Docker: a native LiveKit media
server on `127.0.0.1:7880` and this sidecar on `127.0.0.1:7890`. On first use it
generates and preserves the sidecar bearer token plus a LiveKit API key ID and
secret in `.env`, then prints the exact values needed by the Pack process and
the workspace voice-provider form. It requires `livekit-server` on `PATH`
(`brew install livekit` on macOS, or LiveKit's native installer on Linux). Run
`make dev-config` to print those saved values again without restarting either
process.

Use `make dev-sidecar` when a LiveKit server is already managed separately.
`make run` also starts only this service and sources `.env` when there is one.
Without a file, pass its token on the command line — as a prefix, **not** with
`&&`, which would assign a shell variable the service never sees:

```sh
REALTIME_KIT_TOKEN=$(openssl rand -hex 32) make run
```

| Variable                        | Default | Meaning                                       |
| ------------------------------- | ------- | --------------------------------------------- |
| `REALTIME_KIT_TOKEN`            | —       | **Required.** Shared bearer token, 32+ chars. |
| `REALTIME_KIT_ADDR`             | `:7890` | Listen address (7880 is LiveKit's own).       |
| `REALTIME_KIT_UPSTREAM_TIMEOUT` | `10s`   | Bound on one call to LiveKit.                 |
| `REALTIME_KIT_READ_TIMEOUT`     | `15s`   | HTTP read timeout.                            |
| `REALTIME_KIT_WRITE_TIMEOUT`    | `30s`   | HTTP write timeout.                           |
| `REALTIME_KIT_SHUTDOWN_GRACE`   | `10s`   | How long in-flight requests get on SIGTERM.   |
| `REALTIME_KIT_DEBUG`            | `false` | Debug-level logging (still no credentials).   |

Durations accept either Go syntax (`10s`, `2m`) or a bare number of seconds.
With no token set the service refuses to boot: an unauthenticated instance is a
LiveKit token minter open to whoever can reach the port.

### Native Linux deployment

There are two different installation targets. Both are native systemd services;
neither uses Docker.

On every **Pack application instance**, install the lightweight gateway beside
Pack:

```sh
curl -fsSL https://raw.githubusercontent.com/hushkey-app/realtime-kit/main/install_sidecar.sh | sudo sh
```

The installer selects the Linux `amd64` or `arm64` release, verifies its
checksum, creates an unprivileged `realtime-kit` user, writes
`/etc/realtime-kit/realtime-kit.env`, and starts the service. It generates the
sidecar bearer token on the first install and never replaces it on an update.
Pack must be given that same `REALTIME_KIT_TOKEN` when it calls this service.

On the dedicated **LiveKit media instance**, install the upstream native LiveKit
binary:

```sh
curl -fsSL https://raw.githubusercontent.com/hushkey-app/realtime-kit/main/install_server.sh | sudo sh
```

The media installer downloads an official LiveKit release and verifies it
against LiveKit's published checksum, creates a `livekit` system user, generates
an API key/secret on first install, writes `/etc/livekit/livekit.yaml`, and
starts `livekit-server.service`. Rerunning it updates the binary while
preserving the configuration and credentials. Credentials are not printed into
install or cloud-init logs; inspect the protected configuration interactively as
root. The installer prints the remaining network work explicitly: trusted TLS
for signalling, TCP 7881, the UDP media range, and TURN/TLS. Those depend on the
server's domain and firewall and are deliberately not guessed by the installer.

Update to the latest release with:

```sh
curl -fsSL https://raw.githubusercontent.com/hushkey-app/realtime-kit/main/update.sh | sh
```

Pin the installer or updater by passing the version to the shell that executes
it:

```sh
curl -fsSL https://raw.githubusercontent.com/hushkey-app/realtime-kit/main/install_sidecar.sh | sudo env VERSION=v1.2.3 sh
curl -fsSL https://raw.githubusercontent.com/hushkey-app/realtime-kit/main/update.sh | VERSION=v1.2.3 sh
```

The update is an atomic binary replacement followed by a systemd restart; the
environment file is preserved.

The token in that environment file is **not** a LiveKit API key or secret. It
authenticates Pack to this private sidecar. Each workspace's LiveKit URL, API
key ID and API secret continue to arrive in the `/v1` request body, so this
server remains stateless and multi-tenant.

## API

Every `/v1` route is `POST`, takes JSON, answers JSON, and requires
`Authorization: Bearer $REALTIME_KIT_TOKEN`. Unknown fields are rejected, so a
misspelled key fails here instead of turning into a puzzling 401 from LiveKit.

Credentials are the same object everywhere:

```json
{ "url": "wss://your.livekit.cloud", "apiKey": "APIxxx", "apiSecret": "…" }
```

`url` may be `ws://`, `wss://`, `http://` or `https://` — the SDK normalises it
for the server API, and it is echoed back unchanged for the browser to connect
with.

### `POST /v1/sessions` — ensure a room and mint a join token

The join path, one round trip. `CreateRoom` is idempotent, so this is safe to
call on every join, and it works whether or not the deployment allows LiveKit's
auto-create.

```jsonc
// request
{
  "credentials": { "url": "…", "apiKey": "…", "apiSecret": "…" },
  "room": "channel-abc123",     // required; the caller owns this namespace
  "identity": "user-42",        // required; set it to YOUR user id
  "name": "Ada Lovelace",       // shown on LiveKit's own surfaces
  "metadata": "{\"avatarUrl\":\"…\"}",  // rides on the participant, visible to the room
  "roomMetadata": "…",          // set at room creation only
  "admin": true,                // roomAdmin: mute/remove others. Hosts only.
  "canPublish": true,           // default true
  "canSubscribe": true,         // default true
  "ttlSeconds": 1800,           // default 1800, capped at 6h
  "emptyTimeoutSeconds": 300,   // room creation only; default 300
  "maxParticipants": 0          // room creation only; 0 = LiveKit's default
}

// response
{
  "url": "wss://your.livekit.cloud",
  "room": "channel-abc123",
  "roomSid": "RM_…",
  "token": "eyJ…",
  "expiresAt": 1756612345678,   // Unix ms
  "identity": "user-42"
}
```

### `POST /v1/verify` — prove a credential set works

The cheapest authenticated read LiveKit offers (`ListRooms`). This is the
save-path smoke test: show the user whatever it refuses with.

```jsonc
// request  { "credentials": { … } }
// response { "ok": true, "rooms": 3 }
```

### `POST /v1/rooms/delete` — end a room

```jsonc
// request  { "credentials": { … }, "room": "channel-abc123" }
// response { "ok": true }
```

A room LiveKit has already forgotten is **not** an error: callers delete on
channel deletion and on provider switch, both of which routinely run after the
room has aged out.

### `POST /v1/rooms/participants` — who LiveKit has in a room

For reconciliation and support, not as a source of truth — the caller's own
roster stays authoritative for presence.

```jsonc
// request  { "credentials": { … }, "room": "channel-abc123" }
// response { "participants": [ { "identity": "user-42", "sid": "PA_…",
//                               "name": "Ada", "state": "ACTIVE",
//                               "metadata": "…", "joinedAt": 1756612345 } ] }
```

### `GET /healthz` — no auth

```json
{ "ok": true, "service": "realtime-kit" }
```

### Errors

```json
{
  "error": {
    "code": "invalid_credentials",
    "message": "LiveKit rejected these credentials"
  }
}
```

| Code                   | Status | When                                             |
| ---------------------- | ------ | ------------------------------------------------ |
| `unauthorized`         | 401    | The sidecar's own bearer token is missing/wrong. |
| `invalid_request`      | 400    | Bad JSON, unknown field, missing argument.       |
| `invalid_credentials`  | 401    | LiveKit rejected the key.                        |
| `not_found`            | 404    | LiveKit has no such room.                        |
| `upstream_unavailable` | 503    | LiveKit could not be reached.                    |
| `upstream_error`       | 502    | LiveKit refused for some other reason.           |

Messages are LiveKit's own words for its refusals, and fixed strings for
transport faults — a transport error's text can name the request, and the
request carries the API secret in a header.

## Layout

```
main.go                 boot, signals, graceful shutdown
internal/config         environment → Config, and the fail-closed token check
internal/livekit        the SDK wrapper: Session, DeleteRoom, Participants, Verify
internal/api            routes, bearer auth, JSON envelope, access log
```

`internal/livekit` caches one `*lksdk.LiveKitAPI` per credential set (keyed by a
hash, so no secret sits in a map key), capped and evicted oldest-first, so a hot
channel reuses a connection pool instead of dialling per join.

## Tests

```sh
make test
```

They run against a fake LiveKit twirp server rather than a real deployment, and
cover the join path end to end: the minted token is parsed and verified against
the secret that was sent, its grants are asserted, and every error path is
checked for credential leakage against the exact secret the fake recorded
receiving.
