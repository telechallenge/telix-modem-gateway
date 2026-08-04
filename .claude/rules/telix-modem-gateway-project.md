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
  - **Non-zero ZRINIT buffer (DSZ/GSZ, Searchlight):** zmodem.js only supports streaming receivers (buffer size 0) and throws `Buffer size unsupported` on any other ZRINIT — the Sentry swallows the throw, so uploads to a DOS BBS never even open the picker. `adaptZrinitForSender` in `zmodem-sentry.js` rewrites an inbound ZRINIT to advertise buffer 0 (preserving the capability flags, recomputing the CRC) before it reaches the Sentry. Vendored library stays untouched
  - **Honouring the advertised window (`applyWindowedSend`):** the rewrite above is a lie and only half a fix — the receiver asked for windowing and we told it that it didn't. zmodem.js's sender is firehose-only by design ("Sender opens the firehose … all ZCRCG (!end/!ack) until the end"), so nothing else paces the send and the entire file goes out in one burst. Searchlight BBS 5.1 advertises `buffer=8192`, cancels the overrun with `CAN`×8 and drops carrier; **the browser still shows 100%**, because progress counts bytes handed to zmodem.js, not bytes acknowledged. `applyWindowedSend` overrides `_send_interim_file_piece` on the session to send at most `windowSize` bytes, close the frame with `ZCRCW` (`end_ack`), and wait for the `ZACK` — resetting `_sent_ZDATA` so the next frame re-announces its offset. `readZrinitWindow` must run *before* `adaptZrinitForSender`, which zeroes the field it reads; only a non-zero reading updates it, since the ZRINIT answering our ZSINIT may report 0 and dropping back to firehose mid-file is the bug itself. Two traps: `send_offer` binds the send method when it builds the Transfer, so the override must be installed first; and `Transfer.send()` **discards** its send function's return value, so the in-flight ZACK wait is parked on the session and awaited via `sendChunk`. Regression-tested against real zmodem.js (`a windowed receiver is paced…`, plus a streaming counter-case) and end-to-end against a real `rz` whose ZRINIT is rewritten to advertise a window
  - **A file smaller than the window (`_ackFinalPiece`):** pacing to the window is not enough on its own, because a file *shorter* than the advertised window never fills it — so no frame ever closes with `ZCRCW` and the whole upload goes out unacknowledged (every subpacket `ZCRCG`, the closing one `ZCRCE`, both "no ack" — zmodem.js: *"Sender opens the firehose … all ZCRCG until the end, when we send a ZCRCE"*). The sender then holds **no evidence at all** that a single byte landed: the only thing it waits for is the ZRINIT after ZEOF, and that is byte-identical to the ZRINIT a receiver re-announces when it has given up. A 4279-byte upload to Searchlight that arrived nowhere therefore printed `receiver acknowledged the file` / `session closed cleanly` with the browser at 100% — the same false success the windowing work was meant to end. `handleSend` now marks the file's last slice so it closes with `ZCRCW`, and refuses to report success unless a ZACK actually came back (`_ackCount`), with the ZACK's offset corroborating when the receiver reports a real one. A bare ZACK carries offset 0 — zmodem.js defaults `_bytes4` to four zero bytes and `ZACK_HEADER extends Header`, not `ZOffsetHeader`, so there is no `get_offset()` and the *count* is the load-bearing signal. Regression-tested both ways (a stale ZRINIT must not read as success; a ZACK must complete), and live against `rz` at 4279 bytes
  - **A refused offer is not a delivered file (ZSKIP):** `send_offer` resolves `undefined` for exactly one reason — the receiver answered the ZFILE with `ZSKIP` — and `handleSend` reported that as `endXfer({status:'done'})`, i.e. a successful upload, when the BBS had refused the file before a single data byte was sent. Searchlight prints `Checking for duplicate filenames...` before the offer and skips a name it already holds, so this is exactly what a *repeat* upload of the same file looks like — and the `NO CARRIER` that follows is its own `Disconnect after Upload?` setting, not a fault. The skip now traces the refusal, surfaces an error bubble, and ends the strip as `⊘ REFUSED` via a distinct `skipped` status. Reading a ZSKIP as success is what made a duplicate-name refusal indistinguishable from the windowing bug above
  - **1 KiB data subpackets (`CHUNK` in `handleSend`):** ZMODEM specifies 1 KiB data subpackets; zmodem.js allows 8192 because lrzsz does (`MAX_CHUNK_LENGTH = 8192, //1 KiB officially, but lrzsz allows 8192`), and we sliced at 4 KB — four times the legal maximum. lrzsz accepts that, so every test against `rz` passed. **Searchlight BBS 5.1 does not:** it answers the ZEOF with `ZRPOS 0` ("start over") and loops there until the transfer is cancelled, so the file never lands and the browser still reports 100%. Content-independent — ANSI, ZIP and plain ASCII all failed identically. Confirmed against the live BBS: 4 KB slices fail every time, 1 KiB slices give `Received file ... successfully.` Regression-tested by measuring the longest run of an unescapable payload byte on the wire
  - **Surviving an unexpected header:** zmodem.js throws on any header its current `_next_header_handler` does not list, and the throw escapes `bridge.consume()` into the WebSocket message handler where nothing catches it — the bridge stops pumping and the terminal dies with nothing on screen, indistinguishable from the BBS hanging up. `feed()` catches it, reports via `trace()`, ends the transfer and rebuilds the Sentry. Reachable in practice: Searchlight answers a rejected ZEOF with `ZRPOS` while we wait on `ZRINIT`
  - **Diagnosing a windowed receiver:** the ZRINIT echoed to the terminal is the *post*-rewrite header, so a BBS asking for 8192 and one asking for nothing both display as `0100000023be50`. `feed()` therefore logs the pre-rewrite header to both the console and the terminal (`[zmodem] ZRINIT buffer=… raw=<…>`); the terminal copy is the one that survives being pasted into a bug report
  - **Upload picker needs a user gesture:** a file `<input>` only opens its dialog during transient user activation, which a WebSocket frame (the BBS's ZRINIT) doesn't have — so `promptUpload` surfaces a "Choose files" button and opens the picker from *its* click, mirroring the download Save button. Calling `input.click()` straight from the network event is silently blocked and no dialog appears
- **Cache-busting:** `web/server.js` serves `index.html` with `Cache-Control: no-cache` and stamps `?v=<content-hash>` onto every local `js/*`/`*.css` URL (`web/asset-version.js`). Without this, a browser or the Cloudflare edge keeps serving cached JS after a redeploy, stranding users on old code — the symptom is a fix that's present in the container but not running in the browser. CDN (`https://`) URLs are left unstamped. To confirm which bundle a browser is running, check the DevTools Network tab for the `?v=` query and grep the served file
- **Terminal sizing:** the grid is pinned at 80×24 — BBS ANSI art is authored for 80 columns, so filling the window means growing the cell, never adding columns. `app.js` fits the assembly to 90% of the viewport by setting one `--crt-scale` custom property, which every dimension in `terminal.css` is expressed against (`--u`), and moving xterm's font size in step — so glyphs are rasterized at their final size instead of being upscaled from a 16px canvas. `--crt-scale: 1` in the CSS is the no-JS fallback. The fit measures and corrects across animation frames rather than solving outright (xterm resizes its screen element on its own schedule), and **steps in whole pixels of font size**: xterm's cell metrics are integers, so across 24 rows the height moves in ~24px jumps and a fractional step can leave the assembly a hair over target with no step small enough to correct — the loop then grinds without converging. Growth also stops once a pass has shrunk, so it settles on the side that fits. Cost is landing up to one step under 90%; a whole-pixel size renders the bitmap font cleanly besides
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
