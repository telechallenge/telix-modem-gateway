package server

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	return newDataLoopSessionLogging(t, "")
}

// newDataLoopSessionLogging is the same but sends the session's log to a file
// the caller can read back. Going through the ordinary file output rather than
// a writer seam keeps this from being an API that exists only for tests.
func newDataLoopSessionLogging(t *testing.T, logPath string) (client net.Conn, remote net.Conn) {
	t.Helper()
	return newDataLoopSessionTelnet(t, logPath, "")
}

// newDataLoopSessionTelnet also sets the dialled entry's IAC-doubling policy.
func newDataLoopSessionTelnet(t *testing.T, logPath, telnetMode string) (client net.Conn, remote net.Conn) {
	t.Helper()
	level, format := "error", "text"
	if logPath != "" {
		level, format = "info", "json"
	}
	logger, err := logging.New(level, format, logPath)
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
		dialTelnet:   telnetMode,
		done:         make(chan struct{}),
	}
	s.modem.SetState(modem.StateData)

	sessionsByConn[tConn] = s
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

// sessionsByConn lets a test reach the Session behind a client pipe, so it can
// assert on modem state without the helper returning a wider tuple everywhere.
var sessionsByConn = map[net.Conn]*Session{}

func sessionOf(t *testing.T, client net.Conn) *Session {
	t.Helper()
	s := sessionsByConn[client]
	if s == nil {
		t.Fatal("no session for that client pipe")
	}
	return s
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

// Whether the gateway doubled 0xFF is invisible from both ends, yet it decides
// whether a binary upload survives — a BBS whose front end eats IAC without ever
// negotiating gets undoubled 0xFF and swallows it with the byte behind it, which
// looks exactly like a ZMODEM receiver rejecting every data subpacket while the
// plain-ASCII headers sail through. So the verdict has to be recoverable from
// the log when a real board misbehaves.
func TestDataLoop_LogsWhetherItDoubledIAC(t *testing.T) {
	for _, tc := range []struct {
		name      string
		negotiate bool
		want      string
	}{
		{"telnet peer", true, `"peer_speaks_telnet":"true"`},
		{"raw peer", false, `"peer_speaks_telnet":"false"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "session.log")
			client, remote := newDataLoopSessionLogging(t, logPath)
			if tc.negotiate {
				if _, err := remote.Write([]byte{dialer.IAC, dialer.WILL, dialer.SUPPRESS_GO_AHEAD}); err != nil {
					t.Fatal(err)
				}
				readFor(t, remote, 300*time.Millisecond)
			}
			if _, err := client.Write([]byte{'a', 0xFF, 'b'}); err != nil {
				t.Fatal(err)
			}
			readFor(t, remote, 600*time.Millisecond)

			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			got := string(raw)
			if !strings.Contains(got, `"event":"outbound_iac"`) {
				t.Fatalf("no outbound_iac log entry; got %q", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("want %s in log, got %q", tc.want, got)
			}
			if strings.Count(got, `"event":"outbound_iac"`) != 1 {
				t.Errorf("expected exactly one entry per session, got %q", got)
			}
		})
	}
}

// The inference is right for anything modern but wrong for a 1990s DOS board
// behind a telnet front end: that eats IAC without ever negotiating, so "auto"
// concludes "raw peer", sends 0xFF undoubled and the front end swallows it with
// the byte behind it. ZMODEM cannot escape 0xFF in-band, so the phonebook entry
// has to be able to overrule the inference either way.
func TestDataLoop_PhonebookTelnetOverridesTheInference(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		negotiate bool
		want      []byte
	}{
		{"yes forces doubling on a board that never negotiates", "yes", false, []byte{'a', dialer.IAC, dialer.IAC, 'b'}},
		{"no suppresses it even for a board that did negotiate", "no", true, []byte{'a', 0xFF, 'b'}},
		{"auto still follows the peer", "auto", true, []byte{'a', dialer.IAC, dialer.IAC, 'b'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, remote := newDataLoopSessionTelnet(t, "", tc.mode)
			if tc.negotiate {
				if _, err := remote.Write([]byte{dialer.IAC, dialer.WILL, dialer.SUPPRESS_GO_AHEAD}); err != nil {
					t.Fatal(err)
				}
				readFor(t, remote, 300*time.Millisecond)
			}
			if _, err := client.Write([]byte{'a', 0xFF, 'b'}); err != nil {
				t.Fatal(err)
			}
			if got := readFor(t, remote, 600*time.Millisecond); !bytes.Contains(got, tc.want) {
				t.Errorf("telnet=%q: want % 02x in output, got % 02x", tc.mode, tc.want, got)
			}
		})
	}
}

// Binary upload data contains "+++" every few tens of megabytes, and the escape
// detector only needs guard-time silence either side of it to read that as the
// caller asking for command mode. A ZMODEM sender streams continuously, so that
// silence should never occur — except the browser's own backpressure pauses the
// send loop while its socket drains, which manufactures exactly the gaps the
// detector is looking for.
//
// The result is a gateway that quietly leaves data mode mid-transfer: the BBS
// stops receiving, `rz` times out and deletes the partial file, and neither
// socket closes and no protocol error is logged. That is precisely the shape of
// the intermittent large-upload failure.
func TestDataLoop_BinaryPlusPlusPlusDoesNotEscapeMidTransfer(t *testing.T) {
	client, remote := newDataLoopSession(t)

	// Written from a goroutine: net.Pipe is unbuffered, so once the escape fires
	// and dataLoop returns, nothing reads the client side and a direct Write
	// would block forever — which is the bug wearing a different hat.
	go func() {
		// File data already flowing, so lastInput is current.
		client.Write([]byte("DATA"))
		// Backpressure pauses the browser's send loop while its socket drains.
		time.Sleep(1500 * time.Millisecond)
		// The next chunk resumes with ordinary bytes — which is what marks the
		// gap as guard-time silence — and happens to carry "+++".
		client.Write([]byte("Q+++"))
		// Another drain pause, and the detector has its trailing guard time.
		time.Sleep(1500 * time.Millisecond)
		// More file data follows, as it would mid-upload.
		client.SetWriteDeadline(time.Now().Add(time.Second))
		client.Write([]byte("ZMOD"))
	}()
	got := readFor(t, remote, 5*time.Second)

	if !bytes.Contains(got, []byte("+++")) {
		t.Errorf("the +++ in the file never reached the BBS: % 02x", got)
	}
	if !bytes.Contains(got, []byte("ZMOD")) {
		t.Errorf("the transfer stopped after the +++: % 02x", got)
	}
}

// The other half of the guard-time rule: a caller who genuinely pauses and then
// types +++ must still reach command mode. Tightening the check to require
// silence immediately before the plus must not cost that.
func TestDataLoop_TypedEscapeStillReachesCommandMode(t *testing.T) {
	client, remote := newDataLoopSession(t)
	s := sessionOf(t, client)

	go func() {
		client.Write([]byte("DATA"))
		time.Sleep(1500 * time.Millisecond) // guard time before
		client.Write([]byte("+++"))         // typed on its own, after silence
	}()
	readFor(t, remote, 2*time.Second)

	// The trailing guard time is what commits it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.modem.State() != modem.StateCommand {
		time.Sleep(50 * time.Millisecond)
	}
	if s.modem.State() != modem.StateCommand {
		t.Errorf("modem state = %v after a typed +++ escape, want StateCommand", s.modem.State())
	}
}

// The case the guard-time rule alone cannot catch: a chunk of binary file data
// that *begins* with "+++" right after a backpressure pause is byte-for-byte a
// typed escape. Volume is the only thing that separates them — a caller cannot
// type 64 bytes in one read, and a ZMODEM sender does nothing else.
func TestDataLoop_StreamingClientCannotEscapeOnBinaryPlusses(t *testing.T) {
	client, remote := newDataLoopSession(t)
	s := sessionOf(t, client)

	bulk := bytes.Repeat([]byte{'Z'}, 256)
	go func() {
		// Sustained upload traffic, as a transfer produces.
		for i := 0; i < 12; i++ {
			client.Write(bulk)
		}
		// Backpressure pauses the send loop while the socket drains...
		time.Sleep(1500 * time.Millisecond)
		// ...and the next chunk of file data happens to start with +++.
		client.Write([]byte("+++"))
		time.Sleep(1500 * time.Millisecond)
		client.SetWriteDeadline(time.Now().Add(time.Second))
		client.Write(bulk)
	}()
	got := readFor(t, remote, 5*time.Second)

	if s.modem.State() != modem.StateData {
		t.Errorf("a streaming client escaped to command mode on binary +++; state = %v", s.modem.State())
	}
	if !bytes.Contains(got, []byte("+++")) {
		t.Errorf("the +++ never reached the BBS: %d bytes through", len(got))
	}
}
