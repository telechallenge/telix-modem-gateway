# ZMODEM File Transfer in the Web Terminal — Design

**Date:** 2026-07-08
**Status:** Approved
**Scope:** `web/` (Node proxy + browser) and one small note on Node proxy IAC escaping

## 1. Overview

Add ZMODEM upload/download support to the Telix web terminal at `web/`. The BBS drives both directions:

- **Download:** BBS runs `sz <file>` → browser detects ZRQINIT in the stream → receives file(s) into memory → surfaces each completed file as a "Save" notification bubble that triggers a standard browser download on click.
- **Upload:** BBS runs `rz` → browser detects ZRINIT in the stream → opens the native file picker → user selects file(s) → browser sends them to the BBS via `zmodem.js`.

Files stay entirely in the browser — the Node proxy never buffers a transfer. Batch transfers (multiple files in a single ZMODEM session) are supported. Progress is shown in a dedicated LCD-styled status strip on the modem chrome. Only ZMODEM is in scope for v1; YMODEM and XMODEM are deferred to a follow-up spec.

## 2. Architecture

```
BBS ─(TCP+telnet)─→ Go gateway (dialer.TelnetFilter) ─(TCP+telnet)─→
                                   Go server (session.clientFilter) ─(TCP)─→
                                                       Node proxy ─(WS binary)─→
                                                                        Browser
                                                                        ├─ zmodem.js Sentry (byte sniffer)
                                                                        ├─ xterm.js (idle byte stream)
                                                                        └─ CP437→Unicode decoder
```

**Key architectural change:** the WebSocket transport becomes always-binary in both directions. The Node proxy stops interpreting bytes as text — it becomes a pure byte pipe with one telnet-related responsibility: IAC-escaping browser→Go bytes (`0xFF → 0xFF 0xFF`) so uploads don't get consumed by `internal/server/session.go`'s client-side telnet filter. Existing Go-side telnet handling (both `dialer.TelnetFilter` on the outbound-to-BBS side and the inbound `clientFilter`) is untouched — it already un-escapes IAC IAC correctly per RFC 854.

**ZMODEM state machine location:** entirely in the browser via a vendored `zmodem.js` (FGasper's library, MIT). The Sentry sits between the incoming WebSocket byte stream and xterm.js: bytes flow to the terminal by default; when the Sentry detects a ZMODEM header (`**\x18B00` and variants), it consumes bytes into a `ZmodemSession` object until the session ends (ZFIN + `OO`), after which bytes resume flowing to xterm.js.

**File lifecycle:**
- **Download:** received chunks buffer as `Uint8Array` → wrapped in a `Blob` on completion → surfaced via a notification with `URL.createObjectURL()`.
- **Upload:** `<input type="file" multiple>` picker → each file read as `Uint8Array` → offered to the `ZmodemSession` in sequence.

## 3. Node Proxy Changes (`web/server.js`)

Three focused changes:

1. **Delete `cp437ToUtf8()` and the CP437 tables.** Send raw bytes as binary WebSocket frames (`ws.send(buffer, { binary: true })`). The CP437 table moves verbatim to a new `web/public/js/cp437.js`.

2. **IAC-escape outbound bytes.** When writing browser→Go, scan for `0xFF` and double it. Runs on all outbound bytes always (not mode-switched) — cost is O(n) with zero allocation when there are no `0xFF` bytes; correctness benefit is that no code path can ever send an unescaped IAC. Does *not* apply to NAWS bytes the proxy constructs itself (those are already correctly formed telnet sequences).

3. **New `GET /config.json` endpoint** exposing `MAX_UPLOAD_BYTES` and `ZMODEM_TIMEOUT_SEC` to the browser.

Everything else in the proxy is unchanged. The proxy retains no ZMODEM awareness. Target line count: under 250.

## 4. Browser Components

New files under `web/public/js/`:

| File | Responsibility |
|------|---------------|
| `cp437.js` | CP437→Unicode table + `cp437ToUtf8(Uint8Array): string`. Moved from `server.js`. |
| `zmodem-sentry.js` | Wraps `zmodem.js` Sentry. Owns the incoming byte pump: non-ZMODEM bytes → CP437 decode → xterm; ZMODEM bytes → session. Owns the outbound byte pump: xterm keystrokes when idle, session-emitted bytes during transfer. |
| `zmodem-ui.js` | Status strip DOM binding, notification bubble creation, file picker invocation, progress updates. Pure UI — no protocol logic. |
| `vendor/zmodem.min.js` | Vendored copy of `zmodem.js` (~40 KB minified). Loaded via `<script>` tag; no bundler. |
| `app.js` (modified) | Wires WebSocket ↔ Sentry ↔ xterm ↔ UI. The Sentry replaces the current direct `ws.onmessage → term.write` call. |

## 5. UI

### 5.1 Modem Chrome Status Strip

A new `<div class="modem-xfer">` element sits between `.modem-leds` and `.modem-mute` in `web/public/index.html`, hidden by default (`display: none`). CSS matches the modem's LCD aesthetic — dark green segment on black, monospace pixel font (existing `PxPlus IBM VGA 8x16`), faint scan-line glow. When a transfer begins, the strip fades in with:

```
▶ RCV  file.zip     45%   [████████░░░░░░░]   2400 CPS   00:04 ETA
```

or for uploads:

```
▲ SND  file.zip     12%   [██░░░░░░░░░░░░░]   1200 CPS   00:14 ETA
```

For batch transfers, the current filename cycles; a small `3/7` counter appears on the right. On session end, the strip fades out after 800 ms. Respects `prefers-reduced-motion` (fade → snap).

### 5.2 Download Notification Bubbles

A stacking `<aside class="xfer-notifications">` fixed to the top-right corner of the CRT viewport. Each completed file becomes a bubble:

```
┌─────────────────────────────────┐
│ ⬇ file.zip  (45 KB)      [Save] │
└─────────────────────────────────┘
```

Clicking `Save` triggers `URL.createObjectURL()` + programmatic `<a download>` click, then removes the bubble and revokes the object URL. Bubbles auto-dismiss after 60 s if unclicked (blob revoked). Filename shown is the sanitized name.

### 5.3 Upload Picker

Triggered by ZRINIT detection. Uses a hidden `<input type="file" multiple>` (multiple allowed to match ZMODEM batch capability). If the user cancels the picker, the browser sends ZABORT and the session ends cleanly. The picker is *not* a persistent button — it only appears reactively.

### 5.4 Filename Sanitization

Applied to any ZFILE-supplied filename before display or download:

- Strip path components (keep only basename).
- Restrict to `[A-Za-z0-9._-]` (replace anything else with `_`).
- Cap length at 128 chars.
- Empty or all-invalid result → `download.bin`.

### 5.5 Design Tokens

All colors reuse the CGA palette already in `app.js` (progress bar = `brightGreen`, background = `background`, ETA = `cyan`). No new fonts.

## 6. Failure Modes

| Scenario | Behavior |
|----------|----------|
| BBS aborts mid-transfer (ZABORT) | Sentry closes session, status strip shows `✕ ABORTED` for 2 s then hides. No file surfaced. Terminal resumes. |
| Browser closes mid-transfer | WebSocket close cascades to Node → Go teardown via existing cleanup path. No client-side action needed. |
| CRC mismatch on a block | `zmodem.js` handles retransmit automatically. Status strip shows unchanged progress; retries are invisible. |
| No bytes for `ZMODEM_TIMEOUT_SEC` (default 30 s) during active session | Sentry aborts locally, sends ZABORT, status strip shows `✕ TIMEOUT`. |
| User cancels file picker (upload) | Browser sends ZABORT, session ends cleanly. |
| User closes/refreshes tab mid-download | Buffered bytes lost (memory-only). No persistence guarantee. |
| Uploaded file exceeds `MAX_UPLOAD_BYTES` | Session start rejects the file, sends ZABORT, shows a red notification bubble. Check runs client-side using `MAX_UPLOAD_BYTES` fetched from `/config.json`. |
| Non-ZMODEM `0xFF` in idle stream | Passes through; xterm.js renders the CP437 glyph. No sentry false-positive (sentry requires the full `**\x18B00` header). |

**Cancel UX:** No cancel button. User cancels by pressing Ctrl-X five times (standard ZMODEM cancel) — delivered through xterm to the session. The status strip shows a subtle `Ctrl-X ×5 to abort` hint after 5 s.

## 7. Configuration

Two new env vars in `web/server.js`, exposed to the browser via `GET /config.json`:

| Var | Default | Purpose |
|-----|---------|---------|
| `MAX_UPLOAD_BYTES` | `1073741824` (1 GB) | Client-side gate for upload size. |
| `ZMODEM_TIMEOUT_SEC` | `30` | Session idle timeout. |

No changes to the Go gateway config. No changes to `configs/telix.yaml`.

## 8. Testing

### 8.1 Unit Tests (Browser Code)

Use `node:test` (built-in, zero new dependencies) run from `web/`.

| Module | Coverage |
|--------|----------|
| `cp437.js` | Byte-for-byte match against known CP437 strings; passthrough of `\r\n\x1b` controls; parity with the current `server.js` table for the full 0x00–0xFF range. |
| Filename sanitizer | Path stripping (`../etc/passwd` → `passwd`), char restriction, length cap, empty → `download.bin` fallback. |
| Upload size gate | Rejects `> MAX_UPLOAD_BYTES` with correct notification; accepts exactly at limit. |

### 8.2 Node Proxy Tests (`web/server.test.js`)

- IAC escaping: `0xFF` → `0xFF 0xFF`; multi-byte messages with 0/1/many IACs.
- Binary passthrough: bytes in = bytes out (both directions).
- `/config.json` returns the expected shape.

### 8.3 Integration / E2E

- **Automated (loopback):** `web/e2e/zmodem.spec.js`. A fake BBS server (Node TCP listener) runs `lrzsz`'s `sz` binary against a fixture file. Playwright-cli (with `-s="$PILOT_SESSION_ID"`) drives the browser to receive; the test asserts the notification appears, clicks Save, and verifies the downloaded blob matches the fixture byte-for-byte. Upload path mirrors this with fake BBS running `rz`.
- **Manual smoke:** README section pointing at a real BBS with `sz`/`rz` available.

### 8.4 Explicitly Not Tested

- CRC-16 vs CRC-32 selection (delegated to `zmodem.js`).
- Retransmit behavior on corrupted lines (delegated to `zmodem.js`).
- Resume/ZRPOS (out of scope for v1).

## 9. Non-Goals for v1

- YMODEM / XMODEM (separate spec later).
- Transfer resume (ZRPOS) — no browser persistence for partial state.
- Server-side scanning of uploaded files — files never touch Node disk.
- Per-transfer bandwidth rate limiting — bandwidth budget lives at the Node proxy TCP layer.

## 10. Files Touched

**New:**
- `web/public/js/cp437.js`
- `web/public/js/zmodem-sentry.js`
- `web/public/js/zmodem-ui.js`
- `web/public/js/vendor/zmodem.min.js`
- `web/server.test.js`
- `web/e2e/zmodem.spec.js`

**Modified:**
- `web/server.js` (remove CP437, add IAC escape, add `/config.json`)
- `web/public/index.html` (script tags, status strip element, notifications aside, hidden file input)
- `web/public/js/app.js` (Sentry wiring)
- `web/public/css/terminal.css` (add `.modem-xfer`, `.xfer-notifications` styles)
- `Makefile` (add `web-test` target)

**Unchanged:**
- All of `cmd/`, `internal/`, `configs/`. No Go-side changes.
