package server

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"telix/internal/dialer"
	"telix/internal/logging"
	"telix/internal/modem"
)

// newPipeSession returns a Session wired to one end of a net.Pipe, plus the
// other end for the test to read from.
func newPipeSession(t *testing.T) (*Session, net.Conn) {
	t.Helper()
	logger, err := logging.New("error", "text", "")
	if err != nil {
		t.Fatal(err)
	}
	sConn, tConn := net.Pipe()
	s := &Session{
		conn:         sConn,
		modem:        modem.New("test"),
		logger:       logger.WithSession("test-session", "127.0.0.1"),
		clientFilter: dialer.NewTelnetFilter(),
		banner:       "BANNER",
	}
	t.Cleanup(func() {
		sConn.Close()
		tConn.Close()
	})
	return s, tConn
}

// collectOutput runs fn (which triggers writes to the session's conn) and
// returns everything written to the test-side pipe until fn returns and a
// short quiet period elapses.
func collectOutput(t *testing.T, tConn net.Conn, fn func()) string {
	t.Helper()
	var (
		mu  sync.Mutex
		buf strings.Builder
	)
	done := make(chan struct{})
	go func() {
		b := make([]byte, 1024)
		for {
			tConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := tConn.Read(b)
			if n > 0 {
				mu.Lock()
				buf.Write(b[:n])
				mu.Unlock()
			}
			if err != nil {
				select {
				case <-done:
					return
				default:
				}
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					select {
					case <-done:
						return
					default:
						continue
					}
				}
				return
			}
		}
	}()
	fn()
	// Give the reader a moment to drain the final write, then stop.
	time.Sleep(50 * time.Millisecond)
	close(done)
	tConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	io.Copy(io.Discard, tConn) // wake blocked read if any
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}

func TestProcessCommand_CompoundLineEmitsSingleOK(t *testing.T) {
	s, tConn := newPipeSession(t)
	out := collectOutput(t, tConn, func() {
		s.processCommand("AT V1 X4 S11=30 S7=60")
	})
	if got := strings.Count(out, "OK"); got != 1 {
		t.Fatalf("expected exactly one OK in %q, got %d", out, got)
	}
	if strings.Contains(out, "ERROR") {
		t.Fatalf("unexpected ERROR in %q", out)
	}
}

func TestProcessCommand_SingleCommandStillGetsOK(t *testing.T) {
	s, tConn := newPipeSession(t)
	out := collectOutput(t, tConn, func() {
		s.processCommand("ATV1")
	})
	if got := strings.Count(out, "OK"); got != 1 {
		t.Fatalf("expected exactly one OK in %q, got %d", out, got)
	}
}

func TestProcessCommand_CompoundWithInfoPayloadPreservesPayload(t *testing.T) {
	s, tConn := newPipeSession(t)
	// ATI emits a version banner + OK; S12? emits the register value + OK.
	// The full line should show BOTH payloads but only a single trailing OK.
	out := collectOutput(t, tConn, func() {
		s.processCommand("ATV1 I S12?")
	})
	if !strings.Contains(out, "Telix Modem Gateway") {
		t.Errorf("expected ATI banner in %q", out)
	}
	if got := strings.Count(out, "OK"); got != 1 {
		t.Errorf("expected exactly one OK in %q, got %d", out, got)
	}
}

// promptPasswordResult drives promptPassword with a scripted client input
// and returns the accept/reject result plus everything written to the client.
func promptPasswordResult(t *testing.T, want, typed string) (bool, string) {
	t.Helper()
	s, tConn := newPipeSession(t)
	s.reader = bufio.NewReader(s.conn)

	var (
		mu       sync.Mutex
		out      strings.Builder
		accepted bool
		done     = make(chan struct{})
	)

	// Read everything the session writes.
	go func() {
		b := make([]byte, 1024)
		for {
			tConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := tConn.Read(b)
			if n > 0 {
				mu.Lock()
				out.Write(b[:n])
				mu.Unlock()
			}
			if err != nil {
				select {
				case <-done:
					return
				default:
				}
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
		}
	}()

	// Drive promptPassword in a goroutine so we can send bytes to it.
	promptDone := make(chan struct{})
	go func() {
		accepted = s.promptPassword("555-1212", want)
		close(promptDone)
	}()

	// Wait for PASSWORD: prompt to appear before typing.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := strings.Contains(out.String(), "PASSWORD:")
		mu.Unlock()
		if seen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Type the password, then Enter (CR).
	if _, err := tConn.Write([]byte(typed + "\r")); err != nil {
		t.Fatalf("write typed: %v", err)
	}

	<-promptDone
	time.Sleep(50 * time.Millisecond)
	close(done)
	tConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	io.Copy(io.Discard, tConn)

	mu.Lock()
	defer mu.Unlock()
	return accepted, out.String()
}

func TestPromptPassword_CorrectPasswordAccepted(t *testing.T) {
	ok, out := promptPasswordResult(t, "swordfish", "swordfish")
	if !ok {
		t.Fatalf("expected promptPassword to accept correct password, got false; output=%q", out)
	}
	if !strings.Contains(out, "PASSWORD:") {
		t.Errorf("expected PASSWORD: prompt in output, got %q", out)
	}
	// One '*' per character typed — 9 for "swordfish".
	if got := strings.Count(out, "*"); got != len("swordfish") {
		t.Errorf("expected %d asterisks (one per char), got %d in %q", len("swordfish"), got, out)
	}
	// Password itself must never be echoed.
	if strings.Contains(out, "swordfish") {
		t.Errorf("password leaked into echoed output: %q", out)
	}
	// Correct-path emits no result code — the dial simulation follows.
	if strings.Contains(out, "NO CARRIER") {
		t.Errorf("unexpected NO CARRIER on success: %q", out)
	}
}

func TestPromptPassword_WrongPasswordRejectedWithNoCarrier(t *testing.T) {
	ok, out := promptPasswordResult(t, "swordfish", "wrongguess")
	if ok {
		t.Fatalf("expected promptPassword to reject wrong password, got true; output=%q", out)
	}
	if !strings.Contains(out, "NO CARRIER") {
		t.Errorf("expected NO CARRIER on wrong password, got %q", out)
	}
	if strings.Contains(out, "wrongguess") {
		t.Errorf("typed guess leaked into echoed output: %q", out)
	}
}

func TestProcessCommand_UnknownSubcommandEmitsSingleError(t *testing.T) {
	s, tConn := newPipeSession(t)
	out := collectOutput(t, tConn, func() {
		s.processCommand("ATV1 J X4")
	})
	if !strings.Contains(out, "ERROR") {
		t.Errorf("expected ERROR in %q", out)
	}
	if strings.Contains(out, "OK") {
		t.Errorf("unexpected OK in %q", out)
	}
}
