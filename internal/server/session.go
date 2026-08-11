package server

import (
	"bufio"
	"bytes"
	cryptoRand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"telix/internal/config"
	"telix/internal/dialer"
	"telix/internal/logging"
	"telix/internal/metrics"
	"telix/internal/modem"
)

func generateSessionID() string {
	b := make([]byte, 4)
	cryptoRand.Read(b)
	return hex.EncodeToString(b)
}

// CP437 box-drawing characters used in the banner.
const (
	cp437DblHoriz    = "\xCD" // ═
	cp437DblVert     = "\xBA" // ║
	cp437DblTopLeft  = "\xC9" // ╔
	cp437DblTopRight = "\xBB" // ╗
	cp437DblBotLeft  = "\xC8" // ╚
	cp437DblBotRight = "\xBC" // ╝
	cp437DblTeeRight = "\xCC" // ╠
	cp437DblTeeLeft  = "\xB9" // ╣
	cp437Block       = "\xDB" // █
)

func buildBanner(version string) string {
	B := cp437Block       // █
	H := cp437DblHoriz    // ═
	V := cp437DblVert     // ║
	TL := cp437DblTopLeft // ╔
	TR := cp437DblTopRight
	BL := cp437DblBotLeft
	BR := cp437DblBotRight
	ML := cp437DblTeeRight // ╠
	MR := cp437DblTeeLeft  // ╣

	csi := "\x1b["
	cyan := csi + "0;36m"
	yellow := csi + "1;33m"
	white := csi + "1;37m"
	grey := csi + "0;37m"
	green := csi + "0;32m"
	reset := csi + "0m"
	cls := csi + "2J" + csi + "H"

	// 77 inner columns + 2 border chars = 79 total, avoids 80-column auto-wrap
	bar := ""
	for i := 0; i < 77; i++ {
		bar += H
	}

	// pad right-fills s with spaces to width n
	pad := func(s string, n int) string {
		for len(s) < n {
			s += " "
		}
		return s
	}

	// center centers s within n columns
	center := func(s string, n int) string {
		gap := n - len(s)
		if gap <= 0 {
			return s
		}
		return strings.Repeat(" ", gap/2) + s
	}

	row := func(color, text string) string {
		return V + color + pad(text, 77) + cyan + V + "\r\n"
	}

	// Logo block is 39 chars wide; left offset = (77-39)/2 = 19
	// T crossbar line has 0 relative indent, stem lines have 3 relative indent
	o := strings.Repeat(" ", 19) // logo offset
	s := o + "   "               // stem offset (3 extra for T stem alignment)

	return cls + cyan +
		TL + bar + TR + "\r\n" +
		row(yellow, o+B+B+B+B+B+B+B+B+"  "+B+B+B+B+B+B+B+"  "+B+B+"       "+B+B+"  "+B+B+"   "+B+B) +
		row(yellow, s+B+B+"     "+B+B+"       "+B+B+"       "+B+B+"   "+B+B+" "+B+B) +
		row(yellow, s+B+B+"     "+B+B+B+B+B+"    "+B+B+"       "+B+B+"    "+B+B+B) +
		row(yellow, s+B+B+"     "+B+B+"       "+B+B+"       "+B+B+"   "+B+B+" "+B+B) +
		row(yellow, s+B+B+"     "+B+B+B+B+B+B+B+"  "+B+B+B+B+B+B+B+"  "+B+B+"  "+B+B+"   "+B+B) +
		row("", "") +
		row(white, center("Telix Modem Gateway v"+version, 77)) +
		row(grey, center("Hayes Compatible 56000 BPS", 77)) +
		row("", "") +
		ML + bar + MR + "\r\n" +
		row(green, "  Ready to receive commands. Type AT to test, ATDT<number> to dial.") +
		row(green, "  Use +++ (with pause) to escape data mode, ATH to hang up.") +
		BL + bar + BR + "\r\n" +
		reset
}

// Session represents a client session
type Session struct {
	conn         net.Conn
	modem        *modem.Modem
	config       *config.Config
	logger       *logging.SessionLogger
	metrics      *metrics.Metrics // nil when metrics are disabled; all calls are no-ops
	dialer       *dialer.Dialer
	remoteIP     string
	clientFilter *dialer.TelnetFilter // filters telnet commands from the client
	banner       string

	// dialTelnet is the dialled phonebook entry's IAC-doubling policy: auto
	// (infer from negotiation), yes or no. Empty behaves as auto.
	dialTelnet string

	remoteConn net.Conn
	remoteMu   sync.Mutex
	lastInput  time.Time
	lastDial   time.Time

	// reader is the single buffered reader over conn, set by commandLoop.
	// handleDial reuses it for password prompts so byte-level buffering stays
	// consistent with the command reader.
	reader *bufio.Reader

	done chan struct{}
}

// NewSession creates a new session. art supplies the connect banner when a
// directory of ANSI art is mounted; a nil art, or one with nothing to draw,
// falls back to the built-in banner.
func NewSession(conn net.Conn, cfg *config.Config, logger *logging.Logger, art *bannerArt) *Session {
	remoteAddr := conn.RemoteAddr().String()
	// Extract IP without port
	host, _, _ := net.SplitHostPort(remoteAddr)
	if host == "" {
		host = remoteAddr
	}

	timeout := time.Duration(cfg.Server.IdleTimeout) * time.Second
	if timeout == 0 {
		timeout = 300 * time.Second
	}

	sessionID := generateSessionID()

	return &Session{
		conn:         conn,
		modem:        modem.New(cfg.Version),
		config:       cfg,
		logger:       logger.WithSession(sessionID, host),
		dialer:       dialer.New(timeout, cfg.Dialer.ParsedNetworks()),
		remoteIP:     host,
		clientFilter: dialer.NewTelnetFilter(),
		// Chosen once and held for the session, so ATCLS redraws the same
		// piece the caller connected to rather than shuffling under them.
		banner: bannerFor(art, cfg.Version),
		done:   make(chan struct{}),
	}
}

// Run starts the session
func (s *Session) Run() {
	defer s.cleanup()

	// Negotiate telnet options with the client.
	// Tell the client we will handle echo and suppress go-ahead.
	if _, err := s.writeClient([]byte{
		dialer.IAC, dialer.WILL, dialer.ECHO,
		dialer.IAC, dialer.WILL, dialer.SUPPRESS_GO_AHEAD,
		dialer.IAC, dialer.DO, dialer.SUPPRESS_GO_AHEAD,
	}); err != nil {
		return
	}

	// Give the client a moment to send its negotiation responses,
	// then drain them so they don't pollute the command buffer.
	// Note: telnet filtering is continuous throughout the session via
	// clientFilter — this drain is just for the initial negotiation burst.
	time.Sleep(100 * time.Millisecond)
	s.drainClientTelnet()

	// Send banner
	if _, err := s.writeClient([]byte(s.banner)); err != nil {
		return
	}

	// Send initial OK
	if s.sendResult(modem.ResultOK) != nil {
		return
	}

	// Main session loop
	s.commandLoop()
}

// Close terminates the session
func (s *Session) Close() {
	close(s.done)
	s.conn.Close()
	s.hangup()
}

func (s *Session) cleanup() {
	s.hangup()
	s.conn.Close()
}

func (s *Session) hangup() {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()

	if s.remoteConn != nil {
		s.remoteConn.Close()
		s.remoteConn = nil
	}
	s.modem.SetState(modem.StateCommand)
}

func (s *Session) sendResult(code modem.ResultCode) error {
	result := s.modem.FormatResultExternal(code)
	_, err := s.writeClient([]byte(result))
	return err
}

const clientWriteTimeout = 10 * time.Second

// writeClient writes data to the client connection with a deadline
// to prevent Slowloris-style attacks where the client stops reading.
func (s *Session) writeClient(data []byte) (int, error) {
	s.conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
	n, err := s.conn.Write(data)
	s.conn.SetWriteDeadline(time.Time{})
	return n, err
}

// drainClientTelnet reads and discards any pending telnet negotiation
// bytes the client sent in response to our initial negotiation.
func (s *Session) drainClientTelnet() {
	s.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 512)
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			// Run through the filter to process any DO/WILL/WONT/DONT
			_, responses := s.clientFilter.Filter(buf[:n])
			if len(responses) > 0 {
				if _, err := s.writeClient(responses); err != nil {
					break
				}
			}
		}
		if err != nil {
			break
		}
	}
	// Clear the deadline
	s.conn.SetReadDeadline(time.Time{})
}

// readFilteredByte reads the next non-telnet byte from the client.
// Telnet IAC sequences are consumed by the client filter and responses
// are sent back transparently.
func (s *Session) readFilteredByte(reader *bufio.Reader) (byte, error) {
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}

		// Feed one byte at a time through the filter
		filtered, responses := s.clientFilter.Filter([]byte{b})

		// Send any telnet responses back to the client
		if len(responses) > 0 {
			s.writeClient(responses)
		}

		// If the byte was a telnet command it gets consumed; keep reading
		if len(filtered) > 0 {
			return filtered[0], nil
		}
	}
}

func (s *Session) commandLoop() {
	reader := bufio.NewReader(s.conn)
	s.reader = reader
	idleTimeout := time.Duration(s.config.Server.IdleTimeout) * time.Second
	if idleTimeout == 0 {
		idleTimeout = 300 * time.Second
	}

	for {
		select {
		case <-s.done:
			return
		default:
		}

		s.conn.SetReadDeadline(time.Now().Add(idleTimeout))

		switch s.modem.State() {
		case modem.StateCommand:
			line, err := s.readLine(reader)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					s.logger.IdleTimeout()
				}
				return
			}

			s.processCommand(strings.TrimSpace(line))

		case modem.StateData:
			s.dataLoop(reader)
		}
	}
}

// telnetModeOrAuto keeps an unset mode readable in the log.
func telnetModeOrAuto(mode string) string {
	if mode == "" {
		return "auto"
	}
	return mode
}

const maxLineLength = 1024
const commandTimeout = 5 * time.Minute

func (s *Session) readLine(reader *bufio.Reader) (string, error) {
	var line bytes.Buffer
	cr := s.modem.Registers().GetCR()
	lf := s.modem.Registers().GetLF()
	bs := s.modem.Registers().GetBackspace()

	// Set a deadline for the entire command to complete
	s.conn.SetReadDeadline(time.Now().Add(commandTimeout))

	for {
		b, err := s.readFilteredByte(reader)
		if err != nil {
			return "", err
		}

		// Check for line terminator
		if b == cr || b == lf {
			// CR LF is one terminator, not two. Consuming only the CR left the
			// LF in the buffered reader, where whatever read next took it as its
			// own line ending — which made the phonebook password prompt submit
			// an empty password the instant it was drawn and drop the call
			// before the caller had touched the keyboard. Every telnet client
			// ends a line that way (RFC 854), so this bit anyone not using the
			// web terminal, which sends a bare CR.
			//
			// Only ever discard a byte that is already buffered: peeking past
			// what has arrived would block waiting for a pair that may never
			// come. LF is not IAC, so taking it raw cannot desync the filter.
			if b == cr && reader.Buffered() > 0 {
				if next, err := reader.Peek(1); err == nil && next[0] == lf {
					reader.ReadByte()
				}
			}
			if s.modem.Echo() {
				if _, err := s.writeClient([]byte{cr, lf}); err != nil {
					return "", err
				}
			}
			return line.String(), nil
		}

		// Handle backspace (S5 register) and DEL (0x7F, sent by most modern terminals)
		if b == bs || b == 0x7F {
			if line.Len() > 0 {
				data := line.Bytes()
				line.Reset()
				line.Write(data[:len(data)-1])
				if s.modem.Echo() {
					// Move back, overwrite with space, move back
					if _, err := s.writeClient([]byte{bs, ' ', bs}); err != nil {
						return "", err
					}
				}
			}
			continue
		}

		// Echo if enabled
		if s.modem.Echo() {
			if _, err := s.writeClient([]byte{b}); err != nil {
				return "", err
			}
		}

		// Prevent memory exhaustion from unbounded input
		if line.Len() >= maxLineLength {
			return "", fmt.Errorf("command too long")
		}

		line.WriteByte(b)
	}
}

// readPassword reads a single line of input from the client, echoing '*'
// for each character instead of the character itself. Backspace/DEL erases
// the last asterisk. Modem echo setting is ignored — password entry always
// shadow-echoes regardless of ATE0/ATE1.
func (s *Session) readPassword(reader *bufio.Reader) (string, error) {
	var line bytes.Buffer
	cr := s.modem.Registers().GetCR()
	lf := s.modem.Registers().GetLF()
	bs := s.modem.Registers().GetBackspace()

	s.conn.SetReadDeadline(time.Now().Add(commandTimeout))

	for {
		b, err := s.readFilteredByte(reader)
		if err != nil {
			return "", err
		}

		if b == cr || b == lf {
			if _, err := s.writeClient([]byte{cr, lf}); err != nil {
				return "", err
			}
			return line.String(), nil
		}

		if b == bs || b == 0x7F {
			if line.Len() > 0 {
				data := line.Bytes()
				line.Reset()
				line.Write(data[:len(data)-1])
				if _, err := s.writeClient([]byte{bs, ' ', bs}); err != nil {
					return "", err
				}
			}
			continue
		}

		// Ignore other non-printable control characters
		if b < 0x20 {
			continue
		}

		if line.Len() >= maxLineLength {
			return "", fmt.Errorf("password too long")
		}

		if _, err := s.writeClient([]byte{'*'}); err != nil {
			return "", err
		}
		line.WriteByte(b)
	}
}

func (s *Session) processCommand(input string) {
	if input == "" {
		return
	}

	commands := modem.ParseCommandLine(input)

	// Buffer info payloads from intermediate sub-commands; the terminal
	// result code (OK / CONNECT / NO CARRIER / ERROR) is emitted once for
	// the whole line by the last sub-command (or the short-circuiting
	// dial / clear / online / unknown branch).
	var infoOut strings.Builder
	flushInfo := func() bool {
		if infoOut.Len() == 0 {
			return true
		}
		_, err := s.writeClient([]byte(infoOut.String()))
		infoOut.Reset()
		return err == nil
	}

	for i, cmd := range commands {
		isLast := i == len(commands)-1

		if cmd.Type == modem.CmdUnknown {
			s.logger.InvalidCommand(input)
			flushInfo()
			s.sendResult(modem.ResultError)
			return
		}

		// Handle dial command specially
		if cmd.Type == modem.CmdDial {
			if !flushInfo() {
				return
			}
			s.handleDial(cmd.Number)
			return
		}

		// Handle ATCLS (clear screen and redraw banner)
		if cmd.Type == modem.CmdClear {
			if !flushInfo() {
				return
			}
			if _, err := s.writeClient([]byte(s.banner)); err != nil {
				return
			}
			if isLast {
				s.sendResult(modem.ResultOK)
				return
			}
			continue
		}

		// Handle ATO (return to data mode)
		if cmd.Type == modem.CmdOnline {
			if !flushInfo() {
				return
			}
			s.remoteMu.Lock()
			hasRemote := s.remoteConn != nil
			s.remoteMu.Unlock()
			if hasRemote {
				s.modem.SetState(modem.StateData)
				s.sendResult(modem.ResultConnect)
			} else {
				s.sendResult(modem.ResultNoCarrier)
			}
			return
		}

		response, _, _ := s.modem.Execute(cmd)
		if response == "" {
			continue
		}
		if isLast {
			if !flushInfo() {
				return
			}
			if _, err := s.writeClient([]byte(response)); err != nil {
				return
			}
			continue
		}
		// Intermediate command: strip the trailing OK, keep any info payload
		// (e.g. ATI banner, S-register query value, AT&V config dump).
		okSuffix := s.modem.FormatResultExternal(modem.ResultOK)
		infoOut.WriteString(strings.TrimSuffix(response, okSuffix))
	}
}

func (s *Session) dataLoop(reader *bufio.Reader) {
	s.remoteMu.Lock()
	remote := s.remoteConn
	s.remoteMu.Unlock()

	if remote == nil {
		s.modem.SetState(modem.StateCommand)
		s.sendResult(modem.ResultNoCarrier)
		return
	}

	// Remote-facing: decides for itself whether the BBS speaks telnet, so that
	// a raw TCP BBS's undoubled 0xFF survives inside ZMODEM file data.
	filter := dialer.NewRemoteTelnetFilter()
	escapeChar := s.modem.Registers().GetEscapeChar()
	guardTime := time.Duration(s.modem.Registers().GetEscapeGuardTime()) * time.Millisecond

	// Escape detection state.
	// SECURITY: Escape detection only processes bytes from the client read
	// path (s.clientFilter.Filter on client input). The remote→client
	// goroutine below does not touch escape state, so a malicious BBS
	// cannot inject +++ sequences to force an escape.
	escapeCount := 0
	var lastEscapeTime time.Time
	// A caller typing +++ has just been silent; a file transfer has just pushed
	// megabytes. That difference is the only reliable way to tell the two apart,
	// because guard-time silence alone cannot: the browser's backpressure pauses
	// its send loop while the socket drains, so a chunk of binary file data that
	// happens to *begin* with "+++" arrives looking exactly like a typed escape.
	// It costs a 116 MB upload roughly one in every few tens of megabytes.
	//
	// So escapes are only recognised when the client is not plainly streaming. A
	// human cannot type 512 bytes in one read; a ZMODEM sender does nothing else.
	// readBuf below is 256 bytes, so this is "a full-ish read": a transfer keeps
	// it saturated, a caller typing +++ delivers one to three bytes at a time.
	const bulkChunk = 64
	const bulkQuiet = 5 * time.Second
	var lastBulk time.Time

	// appendRemote queues one data byte for the BBS, doubling 0xFF when the peer
	// speaks telnet. The client-side filter has already collapsed the proxy's
	// doubled 0xFF back to a single byte, so without this a literal 0xFF inside
	// an upload reaches a telnet BBS as a bare IAC and is swallowed along with
	// the byte after it.
	// Whether to double is normally inferred from whether the board answered
	// negotiation, but the phonebook entry can overrule that. A 1990s DOS BBS
	// behind a telnet front end eats IAC without ever negotiating, so the
	// inference says "raw" and every 0xFF we send is swallowed along with the
	// byte behind it — and since ZMODEM cannot escape 0xFF in-band, that ruins
	// every binary upload while plain-ASCII ones sail through untouched.
	telnetMode := s.dialTelnet
	doubleIAC := func() bool {
		switch telnetMode {
		case "yes":
			return true
		case "no":
			return false
		default:
			return filter.PeerSpeaksTelnet()
		}
	}

	// The verdict is invisible from both ends and decides whether a binary
	// upload survives, so record it once per session — at the first 0xFF, which
	// is the moment it starts mattering.
	loggedIAC := false
	appendRemote := func(out []byte, b byte) []byte {
		if b != dialer.IAC {
			return append(out, b)
		}
		if !loggedIAC {
			loggedIAC = true
			s.logger.Info().
				Str("event", "outbound_iac").
				Str("telnet_mode", telnetModeOrAuto(telnetMode)).
				Str("peer_speaks_telnet", strconv.FormatBool(filter.PeerSpeaksTelnet())).
				Str("doubled", strconv.FormatBool(doubleIAC())).
				Msg("client sent 0xFF to the BBS")
		}
		if doubleIAC() {
			return append(out, dialer.IAC, dialer.IAC)
		}
		return append(out, b)
	}

	remoteGone := make(chan struct{})
	readerDone := make(chan struct{})

	// Remote to local (remote BBS -> user)
	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		for {
			n, err := remote.Read(buf)
			if err != nil {
				if err != io.EOF {
					s.logger.Debug().
						Str("event", "remote_read_error").
						Err(err).
						Msg("")
				}
				close(remoteGone)
				return
			}

			// Filter telnet commands
			filtered, responses := filter.Filter(buf[:n])

			// Send any telnet responses
			if len(responses) > 0 {
				if _, err := remote.Write(responses); err != nil {
					close(remoteGone)
					return
				}
			}

			// Forward to user
			if len(filtered) > 0 {
				if _, err := s.writeClient(filtered); err != nil {
					close(remoteGone)
					return
				}
				s.metrics.BytesTransferred(metrics.DirectionToClient, len(filtered))
			}
		}
	}()

	// Ensure the reader goroutine exits before we return
	defer func() {
		remote.Close()
		<-readerDone
	}()

	// Local to remote (user -> remote BBS)
	readBuf := make([]byte, 256)
	for {
		select {
		case <-remoteGone:
			s.hangup()
			s.sendResult(modem.ResultNoCarrier)
			return
		case <-s.done:
			return
		default:
		}

		s.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := s.conn.Read(readBuf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Check escape timeout
				now := time.Now()
				if escapeCount > 0 && now.Sub(lastEscapeTime) > guardTime {
					if escapeCount >= 3 {
						// Successful escape!
						s.modem.SetState(modem.StateCommand)
						s.sendResult(modem.ResultOK)
						return
					}
					// Not an escape after all. The withheld characters are
					// ordinary data and still have to reach the BBS — dropping
					// them silently lost any escape char the user typed (or
					// that occurred in an upload) before a pause.
					var pending []byte
					for i := 0; i < escapeCount; i++ {
						pending = appendRemote(pending, escapeChar)
					}
					escapeCount = 0
					if _, err := remote.Write(pending); err != nil {
						s.hangup()
						s.sendResult(modem.ResultNoCarrier)
						return
					}
				}
				continue
			}
			s.hangup()
			return
		}

		if n == 0 {
			continue
		}

		// Filter telnet commands from client input
		filtered, responses := s.clientFilter.Filter(readBuf[:n])
		if len(filtered) >= bulkChunk {
			lastBulk = time.Now()
		}
		if len(responses) > 0 {
			s.writeClient(responses)
		}

		// Accumulate the whole chunk and write it once. Writing a byte at a
		// time put each one in its own TCP segment, and the resulting
		// Nagle/delayed-ACK stalls held uploads to roughly 1 KB/s.
		out := make([]byte, 0, len(filtered)+escapeCount)

		// Process each filtered byte for escape detection
		for _, b := range filtered {
			now := time.Now()

			// Escape sequence detection
			// Gating the whole branch, not its first arm: a `+` arriving while the
			// client streams must take the ordinary data path. Testing it inside
			// sent it to the "subsequent escape char" arm instead, which counted
			// it regardless and reintroduced the very bug.
			if b == escapeChar && now.Sub(lastBulk) >= bulkQuiet {
				if escapeCount == 0 {
					// The guard time has to be silence immediately before *this*
					// byte. It used to be a flag set by whatever non-escape byte
					// last arrived, so a chunk like "Q+++" inherited the pause
					// that preceded the Q and read as an escape even though the
					// Q sat between the silence and the plus.
					//
					// That is how a binary upload dropped the gateway into
					// command mode mid-transfer: file data contains "+++" every
					// few tens of megabytes, and the browser's backpressure
					// pauses its send loop while the socket drains, manufacturing
					// the gaps either side. The BBS then simply stops receiving —
					// `rz` times out and deletes the partial file, no protocol
					// error is logged and neither socket closes, which is why it
					// looked like an intermittent network fault.
					if now.Sub(s.lastInput) > guardTime {
						escapeCount = 1
						lastEscapeTime = now
					} else {
						// No silence before it, so it is ordinary data.
						out = appendRemote(out, b)
					}
				} else {
					// Subsequent escape chars
					if now.Sub(lastEscapeTime) < guardTime {
						escapeCount++
						lastEscapeTime = now
					} else {
						// Too much time between escapes
						escapeCount = 1
						lastEscapeTime = now
					}
				}
			} else {
				// If we had pending escapes, send them through
				for i := 0; i < escapeCount; i++ {
					out = appendRemote(out, escapeChar)
				}
				escapeCount = 0

				// Send current character
				out = appendRemote(out, b)
			}

			s.lastInput = now
		}

		if len(out) > 0 {
			if _, err := remote.Write(out); err != nil {
				s.hangup()
				s.sendResult(modem.ResultNoCarrier)
				return
			}
			s.metrics.BytesTransferred(metrics.DirectionToRemote, len(out))
		}
	}
}
