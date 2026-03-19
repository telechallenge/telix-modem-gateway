# Session Decomposition & TelnetFilter Cleanup — Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract dial orchestration from session.go into dial.go, remove redundant JS TelnetFilter from web client, and clean up web client connection handling.

**Architecture:** Method extraction within the same `server` package — dial-related `*Session` methods move to `internal/server/dial.go`. Web client's `TelnetFilter` class is removed since the Go server already handles telnet negotiation; the web proxy becomes a passthrough that only does CP437→Unicode conversion.

**Tech Stack:** Go 1.24, Node.js, Express, ws (WebSocket)

---

## Chunk 1: Extract dial orchestration to dial.go

### Task 1: Create dial.go with extracted methods

**Files:**
- Create: `internal/server/dial.go`
- Modify: `internal/server/session.go` (remove lines 404–630)

- [ ] **Step 1: Create `internal/server/dial.go` with all dial-related code**

Move these items from `session.go` to `dial.go` (lines 404–630):

```go
package server

import (
	cryptoRand "crypto/rand"
	"fmt"
	"math/rand"
	"net"
	"time"

	"telix/internal/config"
	"telix/internal/dialer"
	"telix/internal/modem"
)

const dialCooldown = 3 * time.Second

func (s *Session) handleDial(number string) {
	// Enforce minimum time between dial attempts
	if time.Since(s.lastDial) < dialCooldown {
		s.sendResult(modem.ResultError)
		return
	}
	s.lastDial = time.Now()

	// Simulate waiting for dial tone (S6 register, default 2 seconds)
	dialToneWait := time.Duration(s.modem.Registers().GetDialToneWait()) * time.Second
	if dialToneWait > 0 {
		time.Sleep(dialToneWait)
	}

	// Look up number in phonebook
	entry := s.config.LookupNumber(number)
	if entry == nil {
		s.logger.InvalidNumber(number)
		s.sendNoAnswer()
		return
	}

	// Check required settings
	if !s.checkRequiredSettings(number, entry) {
		s.sendRings()
		s.sendGarbageConnect()
		return
	}

	// Attempt connection while ringing
	timeout := time.Duration(s.modem.Registers().GetConnectionTimeout()) * time.Second
	d := dialer.New(timeout, s.config.Dialer.ParsedNetworks())

	s.logger.ConnectionAttempt(number, "attempting")

	// Dial in background so we can ring while waiting
	type dialResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan dialResult, 1)
	go func() {
		conn, err := d.Dial(entry.Host, entry.Port)
		ch <- dialResult{conn, err}
	}()

	// Send 1-2 mandatory rings before checking the dial result,
	// so the user always hears ringing before a connection is picked up.
	mandatoryRings := 1 + rand.Intn(2)
	for i := 0; i < mandatoryRings; i++ {
		s.sendResult(modem.ResultRing)
		time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
	}

	// Continue ringing (0-2 more) while checking for dial completion
	extraRings := rand.Intn(3)
	for i := 0; i < extraRings; i++ {
		s.sendResult(modem.ResultRing)
		select {
		case res := <-ch:
			// Connection resolved during ringing
			if res.err != nil {
				s.logger.ConnectionAttempt(number, "failed")
				s.sendResult(modem.ResultNoCarrier)
				return
			}
			s.remoteMu.Lock()
			s.remoteConn = res.conn
			s.remoteMu.Unlock()
			s.modemHandshakePause()
			s.modem.SetState(modem.StateData)
			s.logger.ConnectionAttempt(number, "success")
			s.sendConnect(entry.RequiredSettings.Baud)
			return
		case <-time.After(time.Duration(2500+rand.Intn(1000)) * time.Millisecond):
			// Keep ringing
		}
	}

	// Done ringing, wait for dial to finish with a safety timeout
	var res dialResult
	select {
	case res = <-ch:
	case <-time.After(timeout + 5*time.Second):
		s.logger.ConnectionAttempt(number, "failed")
		s.sendResult(modem.ResultNoCarrier)
		return
	}
	if res.err != nil {
		s.logger.ConnectionAttempt(number, "failed")
		s.sendResult(modem.ResultNoCarrier)
		return
	}

	s.remoteMu.Lock()
	s.remoteConn = res.conn
	s.remoteMu.Unlock()

	s.modemHandshakePause()
	s.modem.SetState(modem.StateData)
	s.logger.ConnectionAttempt(number, "success")
	s.sendConnect(entry.RequiredSettings.Baud)
}

// checkRequiredSettings validates modem settings against phonebook entry requirements.
// Returns true if all settings match (or entry has no requirements).
func (s *Session) checkRequiredSettings(number string, entry *config.PhonebookEntry) bool {
	rs := entry.RequiredSettings
	ok := true

	if rs.Init != "" && !s.modem.HasSentInit(rs.Init) {
		s.logger.MissingSettings(number, "init", rs.Init)
		ok = false
	}

	if rs.Baud != 0 {
		modemBaud := s.modem.GetBaud()
		if modemBaud != rs.Baud {
			s.logger.MissingSettings(number, "baud", fmt.Sprintf("%d (modem: %d)", rs.Baud, modemBaud))
			ok = false
		}
	}

	if rs.ErrorCorrection != nil {
		wantEC := *rs.ErrorCorrection
		haveEC := s.modem.GetErrorCorrection() == 5
		if wantEC != haveEC {
			s.logger.MissingSettings(number, "error_correction", fmt.Sprintf("want %v, have %v", wantEC, haveEC))
			ok = false
		}
	}

	if rs.Compression != nil {
		wantComp := *rs.Compression
		haveComp := s.modem.GetCompression()
		if wantComp != haveComp {
			s.logger.MissingSettings(number, "compression", fmt.Sprintf("want %v, have %v", wantComp, haveComp))
			ok = false
		}
	}

	return ok
}

// modemHandshakePause simulates the carrier detect / protocol negotiation
// delay that real modems exhibit after the remote picks up and before
// reporting CONNECT.  Uses S9 (carrier detect response time) as a base
// plus a random negotiation delay, totaling roughly 2.5-5.5 seconds.
func (s *Session) modemHandshakePause() {
	s9ms := s.modem.Registers().GetCarrierDetectTime() // default 600ms
	time.Sleep(time.Duration(s9ms)*time.Millisecond + time.Duration(2000+rand.Intn(3000))*time.Millisecond)
}

// connectSpeeds is a weighted pool of realistic negotiated line speeds.
// Most connections land at 56000 (V.90), with occasional lower speeds to
// simulate line-quality variation — just like the real thing.
var connectSpeeds = []int{
	56000, 56000, 56000, 56000, 56000, // 50% chance
	33600, 33600,                       // 20%
	31200,                              // 10%
	28800,                              // 10%
	24000,                              // 10%
}

func pickConnectSpeed() int {
	return connectSpeeds[rand.Intn(len(connectSpeeds))]
}

// sendConnect sends a CONNECT result with a realistic negotiated speed.
// If fixedSpeed > 0, that speed is used instead of a random pick.
func (s *Session) sendConnect(fixedSpeed int) {
	speed := fixedSpeed
	if speed <= 0 {
		modemBaud := s.modem.GetBaud()
		if modemBaud > 0 {
			speed = modemBaud
		} else {
			speed = pickConnectSpeed()
		}
	}
	s.writeClient([]byte(s.modem.FormatConnectResult(speed)))
}

// sendRings sends 1-3 RING results spaced ~3 seconds apart, simulating
// the remote phone ringing before a connection is established.
func (s *Session) sendRings() {
	rings := 1 + rand.Intn(3)
	for i := 0; i < rings; i++ {
		s.sendResult(modem.ResultRing)
		time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
	}
}

// sendNoAnswer simulates dialing a number that rings but nobody picks up,
// just like a real modem would before reporting NO ANSWER to the terminal.
func (s *Session) sendNoAnswer() {
	// Send 4-8 RING results spaced ~3 seconds apart, then NO ANSWER
	rings := 4 + rand.Intn(5)
	for i := 0; i < rings; i++ {
		s.sendResult(modem.ResultRing)
		time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
	}
	s.sendResult(modem.ResultNoAnswer)
}

// sendGarbageConnect simulates a failed modem negotiation by sending CONNECT
// followed by random garbage characters (as a real modem would produce when
// connecting with incompatible settings), then NO CARRIER.
func (s *Session) sendGarbageConnect() {
	cr := s.modem.Registers().GetCR()
	lf := s.modem.Registers().GetLF()

	// Send CONNECT with speed as if the modem established a link
	speed := pickConnectSpeed()
	s.writeClient([]byte(s.modem.FormatConnectResult(speed)))

	// Send 2-5 bursts of random garbage with small delays between them
	bursts := 2 + rand.Intn(4)
	for i := 0; i < bursts; i++ {
		// Each burst is 8-32 random bytes from crypto/rand
		burstLen := 8 + rand.Intn(25)
		buf := make([]byte, burstLen)
		cryptoRand.Read(buf)
		s.writeClient(buf)
		time.Sleep(time.Duration(100+rand.Intn(300)) * time.Millisecond)
	}

	// Terminate with CR/LF and NO CARRIER
	s.writeClient([]byte{cr, lf})
	s.sendResult(modem.ResultNoCarrier)
}
```

- [ ] **Step 2: Remove the moved code from session.go**

Delete lines 404–630 from `session.go` (from `const dialCooldown` through the end of `sendGarbageConnect`). Also remove the now-unused import `"math/rand"` from session.go's import block — all `rand.Intn()` calls moved to dial.go. Keep all other imports (`"telix/internal/config"` is still used by the `Session` struct and `NewSession`, `"telix/internal/dialer"` is still used by `clientFilter`, `NewSession`, `dataLoop`).

After removal, session.go should contain: imports, `generateSessionID`, banner code, `Session` struct, `NewSession`, `Run`, `Close`, `cleanup`, `hangup`, `sendResult`, `writeClient`, `drainClientTelnet`, `readFilteredByte`, `commandLoop`, `readLine`, `processCommand`, `dataLoop` — approximately 581 lines.

- [ ] **Step 3: Verify compilation**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix && go build ./...`
Expected: Clean build, no errors.

- [ ] **Step 4: Run existing tests**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix && go test ./...`
Expected: All existing tests pass (96 top-level tests). No changes to test files needed — no existing server tests cover dial logic (only `ratelimit_test.go` exists).

- [ ] **Step 5: Run vet**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix && go vet ./...`
Expected: Clean, no warnings.

- [ ] **Step 6: Commit**

```bash
cd /Users/ken/Projects/telechallenge-2026/bbs/termix
git add internal/server/dial.go internal/server/session.go
git commit -m "refactor: extract dial orchestration to internal/server/dial.go"
```

---

### Task 2: Write unit tests for dial.go helpers

**Files:**
- Create: `internal/server/dial_test.go`

- [ ] **Step 1: Write tests for `pickConnectSpeed`**

```go
package server

import (
	"testing"
)

func TestPickConnectSpeed(t *testing.T) {
	validSpeeds := map[int]bool{
		56000: true, 33600: true, 31200: true, 28800: true, 24000: true,
	}

	// Run enough times to verify we only get valid speeds
	for i := 0; i < 100; i++ {
		speed := pickConnectSpeed()
		if !validSpeeds[speed] {
			t.Errorf("pickConnectSpeed() returned unexpected speed: %d", speed)
		}
	}
}

func TestConnectSpeedsNotEmpty(t *testing.T) {
	if len(connectSpeeds) == 0 {
		t.Fatal("connectSpeeds pool is empty")
	}
}
```

- [ ] **Step 2: Run tests to verify they pass**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix && go test ./internal/server/ -run TestPickConnect -v`
Expected: PASS

- [ ] **Step 3: Write tests for `checkRequiredSettings`**

These tests need a minimal `*Session` with a `*modem.Modem` and a logger. Since `checkRequiredSettings` only reads modem state and calls logger methods, we can construct a lightweight session.

```go
func newTestSession(t *testing.T) *Session {
	t.Helper()
	logger, err := logging.New("error", "text", "")
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		modem:  modem.New("test"),
		logger: logger.WithSession("test-session", "127.0.0.1"),
	}
}

func TestCheckRequiredSettings_NoRequirements(t *testing.T) {
	s := newTestSession(t)
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
	}
	if !s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected true when no requirements set")
	}
}

func TestCheckRequiredSettings_InitRequired_NotSent(t *testing.T) {
	s := newTestSession(t)
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Init: "ATZ",
		},
	}
	if s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected false when required init not sent")
	}
}

func TestCheckRequiredSettings_InitRequired_Sent(t *testing.T) {
	s := newTestSession(t)
	// Send the required init command through the modem
	s.modem.Execute(modem.ParseCommand("ATZ"))
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Init: "ATZ",
		},
	}
	if !s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected true when required init was sent")
	}
}

func TestCheckRequiredSettings_BaudRequired_Wrong(t *testing.T) {
	s := newTestSession(t)
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Baud: 9600,
		},
	}
	// Default baud is 0 (auto), doesn't match 9600
	if s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected false when baud doesn't match")
	}
}

func TestCheckRequiredSettings_BaudRequired_Correct(t *testing.T) {
	s := newTestSession(t)
	// Lock speed to 9600 via AT&N8
	s.modem.Execute(modem.ParseCommand("AT&N8"))
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Baud: 9600,
		},
	}
	if !s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected true when baud matches")
	}
}

func TestCheckRequiredSettings_ErrorCorrectionRequired_Wrong(t *testing.T) {
	s := newTestSession(t)
	// Default error correction is on (&Q5). Require it off.
	ecOff := false
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			ErrorCorrection: &ecOff,
		},
	}
	if s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected false when error correction doesn't match")
	}
}

func TestCheckRequiredSettings_CompressionRequired_Wrong(t *testing.T) {
	s := newTestSession(t)
	// Default compression is on. Require it off.
	compOff := false
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Compression: &compOff,
		},
	}
	if s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected false when compression doesn't match")
	}
}

func TestCheckRequiredSettings_AllRequired_AllCorrect(t *testing.T) {
	s := newTestSession(t)
	s.modem.Execute(modem.ParseCommand("ATZ"))
	s.modem.Execute(modem.ParseCommand("AT&N8"))
	// Defaults: error correction on, compression on
	ecOn := true
	compOn := true
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Init:            "ATZ",
			Baud:            9600,
			ErrorCorrection: &ecOn,
			Compression:     &compOn,
		},
	}
	if !s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected true when all settings match")
	}
}
```

Add these imports to `dial_test.go`:

```go
import (
	"testing"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/modem"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix && go test ./internal/server/ -run TestCheckRequired -v`
Expected: All 8 tests PASS.

- [ ] **Step 5: Run full test suite**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix && go test ./...`
Expected: All tests pass (96 existing + 10 new = 106 top-level tests).

- [ ] **Step 6: Commit**

```bash
cd /Users/ken/Projects/telechallenge-2026/bbs/termix
git add internal/server/dial_test.go
git commit -m "test: add unit tests for dial.go helpers"
```

---

## Chunk 2: Remove JS TelnetFilter and clean up web client

### Task 3: Remove TelnetFilter and clean up web/server.js

**Files:**
- Modify: `web/server.js`

The Go server already handles telnet negotiation (IAC commands, WILL/WONT/DO/DONT, subnegotiation). Data arriving at the web proxy's TCP connection is already clean application data. The JS `TelnetFilter` re-processes data that's already been filtered — removing it simplifies the proxy to a passthrough.

**What stays:**
- CP437→Unicode conversion (`cp437ToUtf8` function and lookup tables)
- Per-IP WebSocket rate limiting
- NAWS subnegotiation construction (this is protocol *generation* from xterm.js resize events, not filtering)
- Express static file serving

- [ ] **Step 1: Remove TelnetFilter class and state constants**

Delete the `TelnetFilter` class (lines 122–228) and its state constants (lines 115–120) from `web/server.js`. Also remove `TTYPE_IS` and `TTYPE_SEND` constants (lines 25–26) — only used by TelnetFilter's `handleSubnegotiation`.

- [ ] **Step 2: Remove TelnetFilter usage in connection handler**

Replace the `tcp.on('data')` handler to pass data directly through CP437→Unicode without filtering:

Before (lines 272–292):
```javascript
  const telnetFilter = new TelnetFilter();
  const tcp = net.createConnection({ host: TELIX_HOST, port: TELIX_PORT }, () => {
    console.log(`Connected to Telix at ${TELIX_HOST}:${TELIX_PORT}`);
  });

  tcp.on('data', (data) => {
    const { filtered, responses } = telnetFilter.filter(data);
    if (responses.length > 0) {
      tcp.write(responses);
    }
    if (filtered.length > 0) {
      const text = cp437ToUtf8(filtered);
      if (ws.readyState === ws.OPEN) {
        ws.send(text);
      }
    }
  });
```

After:
```javascript
  const tcp = net.createConnection({ host: TELIX_HOST, port: TELIX_PORT });

  tcp.on('connect', () => {
    console.log(`Connected to Telix at ${TELIX_HOST}:${TELIX_PORT}`);
  });

  tcp.on('data', (data) => {
    if (ws.readyState === ws.OPEN) {
      ws.send(cp437ToUtf8(data));
    }
  });
```

- [ ] **Step 3: Consolidate connection teardown**

Replace the scattered close/error handlers with a single cleanup function to ensure the IP counter always gets decremented. The existing `tcp.on('error')` handler (lines 294–299) and `tcp.on('close')` handler (lines 301–306) are replaced — their logic merges into cleanup. Remove the separate `ws.on('close')` IP counter decrement (lines 261–268) since cleanup now handles it.

```javascript
  let cleaned = false;
  function cleanup() {
    if (cleaned) return;
    cleaned = true;
    tcp.destroy();
    if (ws.readyState === ws.OPEN || ws.readyState === ws.CONNECTING) {
      ws.close();
    }
    const count = (ipConnections.get(clientIP) || 1) - 1;
    if (count <= 0) {
      ipConnections.delete(clientIP);
    } else {
      ipConnections.set(clientIP, count);
    }
  }

  tcp.on('error', (err) => {
    console.error(`TCP error (${clientIP}):`, err.message);
    cleanup();
  });

  tcp.on('close', () => {
    console.log(`TCP connection closed (${clientIP})`);
    cleanup();
  });

  ws.on('close', () => {
    console.log(`WebSocket client disconnected (${clientIP})`);
    cleanup();
  });

  ws.on('error', (err) => {
    console.error(`WebSocket error (${clientIP}):`, err.message);
    cleanup();
  });
```

- [ ] **Step 4: Write the complete updated server.js**

The final file should contain, in order:
1. Requires and constants (express, http, net, ws, path, env vars)
2. Telnet protocol constants (IAC, SB, SE, NAWS — only what's needed for NAWS generation)
3. CP437→Unicode tables and `cp437ToUtf8` function (unchanged)
4. Per-IP connection limiting (`MAX_WS_PER_IP`, `ipConnections`, `getClientIP`)
5. Express + WebSocket server setup
6. `wss.on('connection')` handler with:
   - IP rate limiting check
   - TCP connection to Telix (with connect/error handling)
   - `tcp.on('data')` — direct CP437→UTF8 passthrough (no filter)
   - `ws.on('message')` — NAWS construction for resize, passthrough for keystrokes
   - `cleanup()` function for consolidated teardown
   - `tcp.on('close')`, `tcp.on('error')`, `ws.on('close')`, `ws.on('error')` — all call cleanup
7. `server.listen`

- [ ] **Step 5: Verify web client starts**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix/web && node server.js &`
Expected: `Telix web client listening on http://localhost:3000` (then kill the process)

- [ ] **Step 6: Run Go tests to confirm no Go-side regressions**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix && go test ./...`
Expected: All tests pass.

- [ ] **Step 7: Commit**

```bash
cd /Users/ken/Projects/telechallenge-2026/bbs/termix
git add web/server.js
git commit -m "refactor: remove redundant JS TelnetFilter, clean up web client connection handling"
```

---

## Chunk 3: Final verification and cleanup

### Task 4: Full verification

- [ ] **Step 1: Run full Go test suite**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix && go test ./... -v`
Expected: All tests pass (106 top-level tests).

- [ ] **Step 2: Run vet and format**

Run: `cd /Users/ken/Projects/telechallenge-2026/bbs/termix && go vet ./... && go fmt ./...`
Expected: Clean, no warnings, no formatting changes.

- [ ] **Step 3: Verify session.go line count**

Run: `wc -l /Users/ken/Projects/telechallenge-2026/bbs/termix/internal/server/session.go`
Expected: approximately 581 lines (±5).

- [ ] **Step 4: Verify dial.go line count**

Run: `wc -l /Users/ken/Projects/telechallenge-2026/bbs/termix/internal/server/dial.go`
Expected: approximately 227 lines (±5).

- [ ] **Step 5: Verify web/server.js has no TelnetFilter references**

Run: `grep -n "TelnetFilter\|telnetFilter\|STATE_IAC\|STATE_COMMAND\|STATE_SB" /Users/ken/Projects/telechallenge-2026/bbs/termix/web/server.js`
Expected: No matches.

- [ ] **Step 6: Update project rules file**

Update `.claude/rules/telix-modem-gateway-project.md`:
1. Add `dial.go` to the directory structure
2. Update the "Key Architecture" section — remove the note about TelnetFilter being "Duplicated in Go and JS" since the JS implementation was removed

```
internal/
  server/                   # TCP listener, session lifecycle, rate limiter, connection tracker
    dial.go                 # Dial orchestration: phonebook lookup, settings validation, ring/connect simulation
```

- [ ] **Step 7: Commit rules update**

```bash
cd /Users/ken/Projects/telechallenge-2026/bbs/termix
git add .claude/rules/telix-modem-gateway-project.md
git commit -m "docs: update project rules to reflect dial.go extraction"
```
