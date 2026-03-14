# Session Decomposition & TelnetFilter Cleanup

**Date:** 2026-03-13
**Status:** Draft
**Type:** Feature (Refactoring)

## Problem

Two issues in the Telix Modem Gateway codebase:

1. **TelnetFilter duplication** — identical IAC state machine implemented in both Go (`internal/dialer/dialer.go`) and JS (`web/server.js`). The Go server already handles telnet negotiation before data reaches the web proxy, making the JS implementation redundant.

2. **session.go over-coupling** — `internal/server/session.go` is ~808 lines handling command dispatch, dial orchestration, data bridging, escape detection, banner building, garbage generation, and result formatting.

## Approach

**TelnetFilter:** Keep web client thin. Go server fully handles telnet negotiation. Remove JS TelnetFilter — web proxy becomes a dumb bridge. CP437-to-Unicode stays in JS (browser needs Unicode).

**session.go:** Method extraction. Pull dial orchestration into `dial.go` in the same `server` package. Methods keep the `*Session` receiver (idiomatic Go — methods on the same type can live in any file within the package). No new types or interfaces, no call-site changes needed.

## Design

### 1. Go — Extract dial orchestration to `internal/server/dial.go`

New file in the `server` package. All dial-related methods and helpers move out of `session.go`, keeping their existing `*Session` receiver where applicable:

| Method/Function | Purpose |
|-----------------|---------|
| `(s *Session) handleDial(number string)` | Full dial flow: phonebook lookup, settings validation, ring simulation, connect/garbage/no-carrier |
| `(s *Session) checkRequiredSettings(number string, entry *config.PhonebookEntry) bool` | Validates init history, baud, error correction, compression against phonebook requirements (currently inline in handleDial — extracted to a method) |
| `(s *Session) modemHandshakePause()` | Simulates carrier detect / protocol negotiation delay using S9 register |
| `(s *Session) sendConnect(fixedSpeed int)` | Sends CONNECT result with negotiated speed (delegates to `modem.FormatConnectResult()` for string formatting) |
| `(s *Session) sendRings()` | Sends 1-3 RING results with realistic spacing |
| `(s *Session) sendNoAnswer()` | Sends 4-8 RINGs then NO ANSWER |
| `(s *Session) sendGarbageConnect()` | Sends CONNECT + random garbage bursts + NO CARRIER (mismatched settings) |
| `pickConnectSpeed() int` | Package-level function — picks from weighted speed pool |
| `connectSpeeds` var | Package-level weighted speed table |
| `dialCooldown` const | Minimum time between dial attempts |

Call sites in session.go remain unchanged — `s.handleDial(cmd.Number)` still works since the receiver type is the same.

**Note:** CONNECT string formatting (`FormatConnectResult`) stays in the `modem` package where it already lives. `sendConnect` in `dial.go` delegates to it — no duplication.

**Result:** session.go drops from ~808 to ~581 lines (~227 lines moved).

### 2. Web client — Remove JS TelnetFilter and clean up

**Remove:**
- `TelnetFilter` class and all its state machine methods from `web/server.js`
- All call sites that pipe data through the TelnetFilter before forwarding

**Keep:**
- CP437-to-Unicode translation (browser needs Unicode, Go banner sends raw CP437)
- Per-IP WebSocket rate limiting
- Express + WebSocket architecture (unchanged)
- **NAWS subnegotiation construction** — the `ws.on('message')` handler that builds NAWS frames (terminal size updates from xterm.js) and writes them as raw telnet bytes to the Go server. This is protocol *generation* (client telling server its window size), not filtering, so it stays even though TelnetFilter is removed.

**Clean up:**
- Consolidate connection teardown: ensure `tcp.on('close')`, `tcp.on('error')`, `ws.on('close')`, and `ws.on('error')` all go through a single cleanup path that decrements the IP connection counter and destroys both sockets
- Add error handling on `net.createConnection()` — currently a failed TCP connection to the Go server can leave the WebSocket open with no backend
- Remove dead code paths exposed by TelnetFilter removal (filter instantiation, state constants only used by the filter)

### 3. Testing

**New unit tests for `dial.go`:**
- `pickConnectSpeed` — returns values from the expected speed pool
- `checkRequiredSettings` — each setting type independently (init, baud, error correction, compression), combined requirements, no requirements (should pass)

**Existing tests:** No existing server-package tests cover dial logic (only `ratelimit_test.go` exists), so no tests need updating.

**Web client:** Manual verification via `make web-dev` — connection works, banner displays (CP437-to-Unicode), dial-through works, disconnect cleanup correct, terminal resize sends NAWS correctly.

**Verification:** `go test ./...` passes, `go vet ./...` clean, manual `telnet localhost 2323` confirms no behavioral regression.

## Out of Scope

- Data bridging / escape detection extraction (future follow-up)
- Banner/result formatting extraction (too small to justify)
- Web client architecture changes (Express + WebSocket stays)
- New features or behavior changes
