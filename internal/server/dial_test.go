package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/modem"
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
	if s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected false when baud doesn't match")
	}
}

func TestCheckRequiredSettings_BaudRequired_Correct(t *testing.T) {
	s := newTestSession(t)
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

// bbsStub is a throwaway listener standing in for a BBS, counting the
// connections it accepts.
type bbsStub struct {
	host string
	port int

	mu    sync.Mutex
	count int
}

func fakeBBS(t *testing.T) *bbsStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	b := &bbsStub{host: "127.0.0.1", port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.count++
			b.mu.Unlock()
			// Drain so the dialer's telnet negotiation write never blocks.
			go io.Copy(io.Discard, c)
		}
	}()
	return b
}

func (b *bbsStub) accepted() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

// waitAccepted polls until n connections have been accepted. The dialer
// returns as soon as the TCP handshake completes, which can beat the
// listener's Accept, so a bare read of the counter is racy.
func (b *bbsStub) waitAccepted(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.accepted() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("BBS accepted %d connection(s), want %d", b.accepted(), n)
}

// clientRecorder drains everything a session writes to the client end of a
// pipe so a test can inspect the output while the session is still running.
type clientRecorder struct {
	mu   sync.Mutex
	buf  strings.Builder
	stop chan struct{}
}

func recordClient(t *testing.T, tConn net.Conn) *clientRecorder {
	t.Helper()
	r := &clientRecorder{stop: make(chan struct{})}
	go func() {
		b := make([]byte, 1024)
		for {
			select {
			case <-r.stop:
				return
			default:
			}
			tConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, err := tConn.Read(b)
			if n > 0 {
				r.mu.Lock()
				r.buf.Write(b[:n])
				r.mu.Unlock()
			}
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
		}
	}()
	t.Cleanup(func() { close(r.stop) })
	return r
}

func (r *clientRecorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *clientRecorder) waitFor(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(r.text(), substr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; output so far: %q", substr, r.text())
}

// gatedSession returns a session ready to run gatedConnect, plus the client
// end of its pipe.
func gatedSession(t *testing.T) (*Session, net.Conn) {
	t.Helper()
	s, tConn := newPipeSession(t)
	s.config = &config.Config{}
	s.reader = bufio.NewReader(s.conn)
	t.Cleanup(s.hangup)
	return s, tConn
}

// The password gate has to read as the BBS's own login: the terminal must see
// CONNECT first, and the outbound call must not be placed until the password
// is accepted.
func TestGatedConnect_PromptFollowsConnectAndGatesTheDial(t *testing.T) {
	t.Run("wrong password never reaches the BBS", func(t *testing.T) {
		bbs := fakeBBS(t)
		s, tConn := gatedSession(t)
		rec := recordClient(t, tConn)
		entry := &config.PhonebookEntry{
			Number: "555-1212", Host: bbs.host, Port: bbs.port, Password: "swordfish",
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			s.gatedConnect("555-1212", entry)
		}()

		rec.waitFor(t, "PASSWORD:")
		if _, err := tConn.Write([]byte("hunter2\r")); err != nil {
			t.Fatalf("write password: %v", err)
		}
		<-done

		out := rec.text()
		connectAt := strings.Index(out, "CONNECT")
		promptAt := strings.Index(out, "PASSWORD:")
		if connectAt < 0 || promptAt < 0 || connectAt > promptAt {
			t.Errorf("expected CONNECT before the PASSWORD: prompt, got %q", out)
		}
		if !strings.Contains(out, "NO CARRIER") {
			t.Errorf("expected NO CARRIER after a wrong password, got %q", out)
		}
		// Give a stray dial time to land before declaring none happened.
		time.Sleep(200 * time.Millisecond)
		if n := bbs.accepted(); n != 0 {
			t.Errorf("a wrong password opened %d connection(s) to the BBS, want 0", n)
		}
		if s.modem.State() != modem.StateCommand {
			t.Errorf("modem state = %v after a rejected password, want StateCommand", s.modem.State())
		}
	})

	t.Run("correct password bridges to the BBS without a second CONNECT", func(t *testing.T) {
		bbs := fakeBBS(t)
		s, tConn := gatedSession(t)
		rec := recordClient(t, tConn)
		entry := &config.PhonebookEntry{
			Number: "555-1212", Host: bbs.host, Port: bbs.port, Password: "swordfish",
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			s.gatedConnect("555-1212", entry)
		}()

		rec.waitFor(t, "PASSWORD:")
		if _, err := tConn.Write([]byte("swordfish\r")); err != nil {
			t.Fatalf("write password: %v", err)
		}
		<-done

		out := rec.text()
		bbs.waitAccepted(t, 1)
		if s.modem.State() != modem.StateData {
			t.Errorf("modem state = %v after a correct password, want StateData", s.modem.State())
		}
		// The terminal has been "connected" since before the prompt, so a
		// second CONNECT would tell it the call was placed twice.
		if got := strings.Count(out, "CONNECT"); got != 1 {
			t.Errorf("expected exactly one CONNECT, got %d in %q", got, out)
		}
		if strings.Contains(out, "NO CARRIER") {
			t.Errorf("unexpected NO CARRIER on the success path: %q", out)
		}
	})
}

func TestIsConnectionRefused(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name: "connection refused",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &os.SyscallError{
					Syscall: "connect",
					Err:     syscall.ECONNREFUSED,
				},
			},
			expected: true,
		},
		{
			name: "connection timeout",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &os.SyscallError{
					Syscall: "connect",
					Err:     syscall.ETIMEDOUT,
				},
			},
			expected: false,
		},
		{
			name:     "generic error",
			err:      fmt.Errorf("some error"),
			expected: false,
		},
		{
			name:     "nil-safe wrapped error",
			err:      errors.New("connection failed"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isConnectionRefused(tt.err)
			if result != tt.expected {
				t.Errorf("isConnectionRefused(%v) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}
