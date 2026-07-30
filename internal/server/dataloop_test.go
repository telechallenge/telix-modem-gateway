package server

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"

	"telix/internal/dialer"
	"telix/internal/logging"
	"telix/internal/modem"
)

// newDataLoopSession wires a Session to a client pipe and a remote pipe and
// puts it in data mode, returning both test-side ends.
func newDataLoopSession(t *testing.T) (client net.Conn, remote net.Conn) {
	t.Helper()
	logger, err := logging.New("error", "text", "")
	if err != nil {
		t.Fatal(err)
	}
	sConn, tConn := net.Pipe()
	sRemote, tRemote := net.Pipe()

	s := &Session{
		conn:         sConn,
		remoteConn:   sRemote,
		modem:        modem.New("test"),
		logger:       logger.WithSession("test-session", "127.0.0.1"),
		clientFilter: dialer.NewTelnetFilter(),
		done:         make(chan struct{}),
	}
	s.modem.SetState(modem.StateData)

	go s.dataLoop(bufio.NewReader(sConn))

	t.Cleanup(func() {
		close(s.done)
		sConn.Close()
		tConn.Close()
		sRemote.Close()
		tRemote.Close()
	})
	return tConn, tRemote
}

// readFor collects everything the remote receives within d.
func readFor(t *testing.T, c net.Conn, d time.Duration) []byte {
	t.Helper()
	var got []byte
	deadline := time.Now().Add(d)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, err := c.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			break
		}
	}
	return got
}

// A telnet BBS proves itself by answering negotiation; from then on 0xFF
// written back to it must be doubled, or the BBS's own telnet layer eats it
// along with the following byte. This is what broke ZMODEM uploads.
func TestDataLoop_DoublesIACForTelnetPeer(t *testing.T) {
	client, remote := newDataLoopSession(t)

	// BBS answers the gateway's negotiation → it speaks telnet.
	if _, err := remote.Write([]byte{dialer.IAC, dialer.WILL, dialer.SUPPRESS_GO_AHEAD}); err != nil {
		t.Fatal(err)
	}
	readFor(t, remote, 300*time.Millisecond) // let the reply settle

	// Client sends a literal 0xFF in upload data (already un-doubled by the
	// client-side filter, so it arrives here as one byte).
	if _, err := client.Write([]byte{'a', 0xFF, 'b'}); err != nil {
		t.Fatal(err)
	}

	got := readFor(t, remote, 600*time.Millisecond)
	if !bytes.Contains(got, []byte{'a', dialer.IAC, dialer.IAC, 'b'}) {
		t.Errorf("telnet peer should receive doubled IAC, got % 02x", got)
	}
}

// A raw TCP peer never negotiates and must receive the byte undoubled.
func TestDataLoop_LeavesIACAloneForRawPeer(t *testing.T) {
	client, remote := newDataLoopSession(t)

	if _, err := client.Write([]byte{'a', 0xFF, 'b'}); err != nil {
		t.Fatal(err)
	}

	got := readFor(t, remote, 600*time.Millisecond)
	if !bytes.Contains(got, []byte{'a', 0xFF, 'b'}) {
		t.Errorf("raw peer should receive the byte unchanged, got % 02x", got)
	}
	if bytes.Contains(got, []byte{dialer.IAC, dialer.IAC}) {
		t.Errorf("raw peer must not see doubled IAC, got % 02x", got)
	}
}

// Escape characters are withheld while the modem decides whether they are a
// "+++" escape. When the guard time proves they were not, they are ordinary
// data and must still be delivered — they used to be dropped on the floor.
func TestDataLoop_WithheldEscapeCharsAreDeliveredNotDropped(t *testing.T) {
	client, remote := newDataLoopSession(t)

	// A plain byte first: withholding only arms after a non-escape byte has
	// established that a guard-time pause preceded it.
	if _, err := client.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if pre := readFor(t, remote, 400*time.Millisecond); !bytes.Equal(pre, []byte("a")) {
		t.Fatalf("setup: expected the BBS to receive %q, got %q", "a", pre)
	}

	// Idle past the guard time, then send a lone escape char. It is withheld
	// while the modem waits to see whether "+++" follows.
	time.Sleep(1300 * time.Millisecond)
	if _, err := client.Write([]byte("+")); err != nil {
		t.Fatal(err)
	}

	// Nothing more arrives, so the guard time expires and it was never an
	// escape — the character is ordinary data and must still be delivered.
	got := readFor(t, remote, 2500*time.Millisecond)
	if !bytes.Contains(got, []byte("+")) {
		t.Errorf("withheld escape char never reached the BBS, got % 02x", got)
	}
}

// The data path must stay byte-exact for a raw peer at chunk sizes above the
// 256-byte read buffer, which is where the batched write path kicks in.
func TestDataLoop_LargeChunkArrivesIntact(t *testing.T) {
	client, remote := newDataLoopSession(t)

	payload := make([]byte, 2000)
	for i := range payload {
		payload[i] = byte((i*7 + 3) & 0xFF)
		if payload[i] == '+' { // keep escape detection out of this test
			payload[i] = 'x'
		}
	}

	go func() { client.Write(payload) }()

	got := readFor(t, remote, 1500*time.Millisecond)
	// 0xFF is passed through untouched for a raw peer, so the stream should
	// match byte for byte.
	if !bytes.Equal(got, payload) {
		t.Errorf("payload corrupted: got %d bytes, want %d", len(got), len(payload))
		for i := 0; i < len(got) && i < len(payload); i++ {
			if got[i] != payload[i] {
				t.Errorf("first difference at %d: got 0x%02x want 0x%02x", i, got[i], payload[i])
				break
			}
		}
	}
}
