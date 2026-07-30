# Project: Telix Modem Gateway

**Last Updated:** 2026-03-13

## Overview

Virtual Hayes-compatible modem gateway. Telnet clients connect, issue AT commands, and dial phone numbers to reach backend BBS systems via TCP. Includes a web terminal client (Node.js/WebSocket → xterm.js).

## Technology Stack

- **Language:** Go 1.24 (gateway), Node.js (web client)
- **Dependencies:** `golang.org/x/time` (rate limiting), `lumberjack.v2` (log rotation), `yaml.v3` (config)
- **Web client:** Express, `ws` (WebSocket), xterm.js (browser terminal)
- **Container:** Docker multi-stage build, Docker Compose (gateway + fail2ban + web client)
- **CI:** SLSA Go releaser (`.github/workflows/go-ossf-slsa3-publish.yml`)

## Directory Structure

```
cmd/telix/main.go          # Entry point — config load, logger init, server start
internal/
  config/                   # YAML config parsing, phonebook lookup, number normalization
  dialer/                   # Outbound TCP+telnet dialer, TelnetFilter (IAC state machine)
  logging/                  # Custom structured logger (JSON/text), log rotation, sanitization
  modem/                    # AT command parser, modem state machine, S-registers, result codes
  server/                   # TCP listener, session lifecycle, rate limiter, connection tracker
    dial.go                 # Dial orchestration: phonebook lookup, settings validation, ring/connect simulation
web/
  server.js                 # Express + WebSocket proxy (bridges browser ↔ Telix via TCP)
  public/                   # Static frontend: xterm.js terminal, CP437 font, CSS
configs/telix.yaml          # Default config (phonebook, rate limits, logging)
fail2ban/                   # fail2ban filter + jail config for abuse prevention
```

## Development Commands

| Task | Command |
|------|---------|
| Build | `make build` |
| Run | `make run` (builds first, uses `configs/telix.yaml`) |
| Test | `make test` or `go test ./...` |
| Vet | `make vet` or `go vet ./...` |
| Format | `make fmt` or `go fmt ./...` |
| Docker (full stack) | `make docker-up` (gateway + fail2ban + web) |
| Docker down | `make docker-down` |
| Web client install | `make web-install` |
| Web client dev | `make web-dev` |
| Connect manually | `telnet localhost 2323` |

## Key Architecture

- **Session lifecycle:** `server.go` accepts TCP → creates `Session` → `session.go` runs command/data loop → `modem` package handles AT parsing + state
- **Modem state:** `StateCommand` (reads AT commands) ↔ `StateData` (bridges client ↔ remote BBS)
- **Phonebook matching:** Numbers normalized (strip `-().# ` etc.), entries can require `init`, `baud`, `error_correction`, `compression` — mismatched settings produce garbage + NO CARRIER
- **Password-gated entries:** an entry with `password` takes the `gatedConnect` path in `dial.go` — RING, then CONNECT, *then* the `PASSWORD:` prompt, so it reads as the BBS's own login rather than a gateway-side gate. The outbound dial is only placed after the password is accepted (a wrong guess never reaches the host), and success enters data mode without a second CONNECT since the terminal already saw one
- **Telnet filtering:** `TelnetFilter` (IAC state machine in `internal/dialer`) filters telnet commands from data streams in both directions. Go server handles all negotiation; web client is a passthrough proxy (CP437→Unicode only)
  - The BBS-facing filter is built with `NewRemoteTelnetFilter()`, which additionally infers whether the peer speaks telnet: a peer that answers negotiation has its doubled `0xFF` un-escaped as usual, while a raw TCP peer's `0xFF` passes through untouched. The verdict is locked once a ZMODEM frame header appears, so file data can never flip it. Client-facing input uses plain `NewTelnetFilter()` — the web proxy always doubles `0xFF`, so that contract is unambiguous
  - **ZMODEM detection:** `zmodem-sentry.js` slices inbound data so each ZMODEM hex header ends a chunk. zmodem.js only fires a detection when its input *ends* with a header, and a coalesced ZRQINIT pair otherwise wedges detection permanently
  - **Partial headers:** a header straddling a TCP chunk boundary is held back in `headerTail` and prepended to the next chunk, in both the detecting and the live-session path. A header only counts as complete once its `CR LF` terminator has arrived — zmodem.js cannot parse one without it. `scheduleTailFlush` releases a fragment after 100ms so nothing is ever swallowed. This is what made batch downloads fail while single-file worked: the longer preamble moved the chunk boundary into the header
  - **ZRQINIT retransmits vs. per-file sessions:** a ZRQINIT arriving during a live receive session is ambiguous. Before the session's file offer arrives it's a retransmit (sender hasn't seen our ZRINIT) — zmodem.js throws `Unhandled header` on it, wedging the download — so `feed()` drops it (`isZrqinitHeader` + `sessionSawOffer === false`). After the offer, the file is already delivering, so a ZRQINIT is the *next* file's session: some BBSes (DOS DSZ/GSZ behind a telnet bridge) drive a multi-file download as one session per file and **never send ZFIN between them**, so the old session never closes itself. `resetSession()` drops the stale session and rebuilds the Sentry (its internal `_zsession` would otherwise still point at the dead session and throw). Without this only the first file arrived. Regression-tested with real zmodem.js on both ends (`per-file sessions without ZFIN`)
  - **Uploads (client→BBS):** `dataLoop` mirrors the read side — `appendRemote` re-doubles `0xFF` when `filter.PeerSpeaksTelnet()`, since the client-side filter has already collapsed the proxy's doubled bytes. The whole read chunk is written in one `Write`; per-byte writes cost ~7× throughput. Escape characters withheld pending a `+++` decision are flushed to the BBS when the guard time proves they were not an escape
  - **ZMODEM uploads:** the send session is only confirmed once the user has picked files (`beginUpload`), because `rz` repeats ZRINIT every ~10s and zmodem.js throws if a live session sees a second header. `files_remaining` must be omitted rather than sent as `0`
  - **Non-zero ZRINIT buffer (DSZ/GSZ):** zmodem.js only supports streaming receivers (buffer size 0) and throws `Buffer size unsupported` on any other ZRINIT — the Sentry swallows the throw, so uploads to a DOS BBS never even open the picker. `neutralizeZrinitBuffer` in `zmodem-sentry.js` rewrites an inbound ZRINIT to advertise buffer 0 (preserving the capability flags, recomputing the CRC) before it reaches the Sentry; the sender then streams and the receiver copes over TCP. Vendored library stays untouched
  - **Upload picker needs a user gesture:** a file `<input>` only opens its dialog during transient user activation, which a WebSocket frame (the BBS's ZRINIT) doesn't have — so `promptUpload` surfaces a "Choose files" button and opens the picker from *its* click, mirroring the download Save button. Calling `input.click()` straight from the network event is silently blocked and no dialog appears
- **Cache-busting:** `web/server.js` serves `index.html` with `Cache-Control: no-cache` and stamps `?v=<content-hash>` onto every local `js/*`/`*.css` URL (`web/asset-version.js`). Without this, a browser or the Cloudflare edge keeps serving cached JS after a redeploy, stranding users on old code — the symptom is a fix that's present in the container but not running in the browser. CDN (`https://`) URLs are left unstamped. To confirm which bundle a browser is running, check the DevTools Network tab for the `?v=` query and grep the served file
- **Security:** Rate limiter (per-IP + global), connection tracker (max total + per-IP), write deadlines (anti-Slowloris), command length limits, log field sanitization, escape sequence injection protection, network-restricted outbound dials (`dialer.allowed_networks`)
- **Logging:** SessionLogger embeds `session_id` + `source_ip` in every log event for correlation
- **CP437 encoding:** Banner uses raw CP437 bytes; web client translates CP437 → Unicode for browser display
- **Version:** Injected at build time via `-ldflags -X main.version=...` from `git describe`. Used in banner, ATI response.
- **Web client:** Per-IP WebSocket connection limiting (default 5, `MAX_WS_PER_IP` env var)

## Testing

- Tests in `internal/config/`, `internal/modem/`, `internal/dialer/`, `internal/server/`
- Table-driven tests with `t.Run()` subtests
- Run: `go test ./...`

## Important Notes

- `required_init` field is deprecated — auto-migrated to `required_settings.init` at config load
- Error correction (`&Q5`) and compression (`%C1`) default to `true` in both modem state and phonebook entries
- S12 (escape guard time) has a minimum of 20 (400ms) to prevent trivial escape attacks
- The web client's CP437→Unicode table must stay in sync with the Go banner's byte values
- `dialer.allowed_networks` restricts outbound dials to configured CIDRs — **use in production** to prevent the gateway from reaching arbitrary hosts
