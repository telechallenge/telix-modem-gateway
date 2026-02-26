package server

import (
	"bufio"
	"bytes"
	cryptoRand "crypto/rand"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"telix/internal/config"
	"telix/internal/dialer"
	"telix/internal/logging"
	"telix/internal/modem"
)

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

// banner is the ANSI art displayed when a client connects, using CP437 encoding.
var banner = buildBanner()

func buildBanner() string {
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

	row := func(color, text string) string {
		return V + color + pad(text, 77) + cyan + V + "\r\n"
	}

	return cls + cyan +
		TL + bar + TR + "\r\n" +
		row(yellow, "      "+B+B+B+B+B+B+B+B+" "+B+B+B+B+B+B+B+" "+B+B+"     "+B+B+" "+B+B+"  "+B+B) +
		row(yellow, "         "+B+B+"   "+B+B+"     "+B+B+"     "+B+B+"  "+B+B+" "+B+B) +
		row(yellow, "         "+B+B+"   "+B+B+B+B+B+"  "+B+B+"     "+B+B+"   "+B+B+B) +
		row(yellow, "         "+B+B+"   "+B+B+"     "+B+B+"     "+B+B+"  "+B+B+" "+B+B) +
		row(yellow, "         "+B+B+"   "+B+B+B+B+B+B+B+" "+B+B+B+B+B+B+B+" "+B+B+" "+B+B+"  "+B+B) +
		row("", "") +
		row(white, "                   Telix Modem Gateway v1.0") +
		row(grey, "                    Hayes Compatible 56000 BPS") +
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
	logger       *logging.Logger
	dialer       *dialer.Dialer
	remoteIP     string
	clientFilter *dialer.TelnetFilter // filters telnet commands from the client

	remoteConn   net.Conn
	remoteMu     sync.Mutex
	inputBuffer  bytes.Buffer
	escapeBuffer bytes.Buffer
	lastInput    time.Time
	lastDial     time.Time

	done chan struct{}
}

// NewSession creates a new session
func NewSession(conn net.Conn, cfg *config.Config, logger *logging.Logger) *Session {
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

	return &Session{
		conn:         conn,
		modem:        modem.New(),
		config:       cfg,
		logger:       logger,
		dialer:       dialer.New(timeout),
		remoteIP:     host,
		clientFilter: dialer.NewTelnetFilter(),
		done:         make(chan struct{}),
	}
}

// Run starts the session
func (s *Session) Run() {
	defer s.cleanup()

	// Negotiate telnet options with the client.
	// Tell the client we will handle echo and suppress go-ahead.
	s.conn.Write([]byte{
		dialer.IAC, dialer.WILL, dialer.ECHO,
		dialer.IAC, dialer.WILL, dialer.SUPPRESS_GO_AHEAD,
		dialer.IAC, dialer.DO, dialer.SUPPRESS_GO_AHEAD,
	})

	// Give the client a moment to send its negotiation responses,
	// then drain them so they don't pollute the command buffer.
	time.Sleep(100 * time.Millisecond)
	s.drainClientTelnet()

	// Send banner
	s.writeClient([]byte(banner))

	// Send initial OK
	s.sendResult(modem.ResultOK)

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

func (s *Session) sendResult(code modem.ResultCode) {
	result := s.modem.FormatResultExternal(code)
	s.writeClient([]byte(result))
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
				s.conn.Write(responses)
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
					s.logger.Info().
						Str("event", "idle_timeout").
						Str("source_ip", s.remoteIP).
						Msg("")
				}
				return
			}

			s.processCommand(strings.TrimSpace(line))

		case modem.StateData:
			s.dataLoop(reader)
		}
	}
}

const maxLineLength = 1024
const commandTimeout = 30 * time.Second

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

		// Echo if enabled
		if s.modem.Echo() {
			s.writeClient([]byte{b})
		}

		// Check for line terminator
		if b == cr || b == lf {
			if s.modem.Echo() {
				// Send CR LF
				s.writeClient([]byte{cr, lf})
			}
			return line.String(), nil
		}

		// Handle backspace
		if b == bs {
			if line.Len() > 0 {
				// Remove last character
				data := line.Bytes()
				line.Reset()
				line.Write(data[:len(data)-1])
				// Send backspace, space, backspace to erase
				if s.modem.Echo() {
					s.writeClient([]byte{' ', bs})
				}
			}
			continue
		}

		// Prevent memory exhaustion from unbounded input
		if line.Len() >= maxLineLength {
			return "", fmt.Errorf("command too long")
		}

		line.WriteByte(b)
	}
}

func (s *Session) processCommand(input string) {
	if input == "" {
		return
	}

	cmd := modem.ParseCommand(input)

	if cmd.Type == modem.CmdUnknown {
		s.logger.InvalidCommand(s.remoteIP, input)
		s.sendResult(modem.ResultError)
		return
	}

	// Handle dial command specially
	if cmd.Type == modem.CmdDial {
		s.handleDial(cmd.Number)
		return
	}

	response, _, _ := s.modem.Execute(cmd)
	if response != "" {
		s.writeClient([]byte(response))
	}
}

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
		s.logger.InvalidNumber(s.remoteIP, number)
		s.sendNoAnswer()
		return
	}

	// Check required settings
	rs := entry.RequiredSettings
	settingsOK := true

	if rs.Init != "" && !s.modem.HasSentInit(rs.Init) {
		s.logger.MissingSettings(s.remoteIP, number, "init", rs.Init)
		settingsOK = false
	}

	if rs.Baud != 0 {
		modemBaud := s.modem.GetBaud()
		if modemBaud != rs.Baud {
			s.logger.MissingSettings(s.remoteIP, number, "baud", fmt.Sprintf("%d (modem: %d)", rs.Baud, modemBaud))
			settingsOK = false
		}
	}

	if rs.ErrorCorrection != nil {
		wantEC := *rs.ErrorCorrection
		haveEC := s.modem.GetErrorCorrection() == 5
		if wantEC != haveEC {
			s.logger.MissingSettings(s.remoteIP, number, "error_correction", fmt.Sprintf("want %v, have %v", wantEC, haveEC))
			settingsOK = false
		}
	}

	if rs.Compression != nil {
		wantComp := *rs.Compression
		haveComp := s.modem.GetCompression()
		if wantComp != haveComp {
			s.logger.MissingSettings(s.remoteIP, number, "compression", fmt.Sprintf("want %v, have %v", wantComp, haveComp))
			settingsOK = false
		}
	}

	if !settingsOK {
		s.sendRings()
		s.sendGarbageConnect()
		return
	}

	// Attempt connection while ringing
	timeout := time.Duration(s.modem.Registers().GetConnectionTimeout()) * time.Second
	d := dialer.New(timeout)

	s.logger.ConnectionAttempt(s.remoteIP, number, "attempting")

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
				s.logger.ConnectionAttempt(s.remoteIP, number, "failed")
				s.sendResult(modem.ResultNoCarrier)
				return
			}
			s.remoteMu.Lock()
			s.remoteConn = res.conn
			s.remoteMu.Unlock()
			s.modemHandshakePause()
			s.modem.SetState(modem.StateData)
			s.logger.ConnectionAttempt(s.remoteIP, number, "success")
			s.sendConnect(rs.Baud)
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
		s.logger.ConnectionAttempt(s.remoteIP, number, "failed")
		s.sendResult(modem.ResultNoCarrier)
		return
	}
	if res.err != nil {
		s.logger.ConnectionAttempt(s.remoteIP, number, "failed")
		s.sendResult(modem.ResultNoCarrier)
		return
	}

	s.remoteMu.Lock()
	s.remoteConn = res.conn
	s.remoteMu.Unlock()

	s.modemHandshakePause()
	s.modem.SetState(modem.StateData)
	s.logger.ConnectionAttempt(s.remoteIP, number, "success")
	s.sendConnect(rs.Baud)
}

// modemHandshakePause simulates the carrier detect / protocol negotiation
// delay that real modems exhibit after the remote picks up and before
// reporting CONNECT.  Uses S9 (carrier detect response time) as a base
// plus a random negotiation delay, totaling roughly 2.5-5.5 seconds.
func (s *Session) modemHandshakePause() {
	s9ms := s.modem.Registers().GetCarrierDetectTime() // default 600ms
	time.Sleep(time.Duration(s9ms)*time.Millisecond + time.Duration(2000+rand.Intn(3000))*time.Millisecond)
}

// connectSpeed picks a realistic negotiated line speed from a weighted pool.
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

func (s *Session) dataLoop(reader *bufio.Reader) {
	s.remoteMu.Lock()
	remote := s.remoteConn
	s.remoteMu.Unlock()

	if remote == nil {
		s.modem.SetState(modem.StateCommand)
		s.sendResult(modem.ResultNoCarrier)
		return
	}

	filter := dialer.NewTelnetFilter()
	escapeChar := s.modem.Registers().GetEscapeChar()
	guardTime := time.Duration(s.modem.Registers().GetEscapeGuardTime()) * time.Millisecond

	// Escape detection state
	escapeCount := 0
	var lastEscapeTime time.Time
	preEscapePause := false

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
				remote.Write(responses)
			}

			// Forward to user
			if len(filtered) > 0 {
				s.writeClient(filtered)
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
					// Reset escape state
					escapeCount = 0
					preEscapePause = false
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
		if len(responses) > 0 {
			s.writeClient(responses)
		}

		// Process each filtered byte for escape detection
		for _, b := range filtered {
			now := time.Now()

			// Escape sequence detection
			if b == escapeChar {
				if escapeCount == 0 {
					// First escape char - need pre-pause
					if preEscapePause {
						escapeCount = 1
						lastEscapeTime = now
					} else {
						// No pre-pause, send char through
						remote.Write([]byte{b})
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
				// Check if we had a guard time pause before this
				if now.Sub(s.lastInput) > guardTime {
					preEscapePause = true
				} else {
					preEscapePause = false
				}

				// If we had pending escapes, send them through
				for i := 0; i < escapeCount; i++ {
					remote.Write([]byte{escapeChar})
				}
				escapeCount = 0

				// Send current character
				remote.Write([]byte{b})
			}

			s.lastInput = now
		}
	}
}
