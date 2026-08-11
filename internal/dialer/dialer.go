package dialer

import (
	"fmt"
	"net"
	"time"
)

// Telnet protocol constants
const (
	IAC  = 255 // Interpret As Command
	DONT = 254
	DO   = 253
	WONT = 252
	WILL = 251
	SB   = 250 // Sub-negotiation Begin
	SE   = 240 // Sub-negotiation End

	// Telnet options
	ECHO              = 1
	SUPPRESS_GO_AHEAD = 3
	TERMINAL_TYPE     = 24
	NAWS              = 31 // Negotiate About Window Size

	// Subnegotiation qualifiers
	TTYPE_IS   = 0
	TTYPE_SEND = 1
)

// ZMODEM framing bytes, used only to recognise the start of a binary transfer.
const (
	ZPAD   = '*'  // frame prefix
	ZDLE   = 0x18 // ZMODEM escape (CAN)
	ZBIN   = 'A'  // binary frame, CRC-16
	ZHEX   = 'B'  // hex frame
	ZBIN32 = 'C'  // binary frame, CRC-32
)

// Dialer handles outbound connections
type Dialer struct {
	timeout         time.Duration
	allowedNetworks []*net.IPNet
}

// New creates a new dialer. If allowedNetworks is non-empty, resolved IPs
// must fall within at least one CIDR or the dial is rejected.
func New(timeout time.Duration, allowedNetworks []*net.IPNet) *Dialer {
	return &Dialer{
		timeout:         timeout,
		allowedNetworks: allowedNetworks,
	}
}

// Dial connects to a remote host with telnet negotiation.
// If allowed networks are configured, the resolved IP is checked first.
func (d *Dialer) Dial(host string, port int) (net.Conn, error) {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	if len(d.allowedNetworks) > 0 {
		if err := d.checkAllowed(host); err != nil {
			return nil, err
		}
	}

	conn, err := net.DialTimeout("tcp", addr, d.timeout)
	if err != nil {
		return nil, err
	}

	// Perform telnet negotiation synchronously before returning
	d.negotiate(conn)

	return conn, nil
}

// Probe reports whether host:port will accept a TCP connection, hanging up
// immediately. Allowed networks bind it exactly as they bind Dial — the one
// thing that contacts every phonebook host on a timer must not be the one thing
// exempt from the setting that exists to stop that.
//
// It deliberately does not negotiate telnet. A BBS treats terminal-type chatter
// as the start of a call and writes it to its caller log, so a negotiating
// probe would fill a board's log with a phantom call every interval. A bare
// connect-and-close is what "is the line answering" means, and it is also all
// this can honestly claim: a board that accepts the socket and then refuses to
// let anybody log in still reads as reachable.
func (d *Dialer) Probe(host string, port int) error {
	if len(d.allowedNetworks) > 0 {
		if err := d.checkAllowed(host); err != nil {
			return err
		}
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)), d.timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// checkAllowed resolves the host and verifies all IPs are within allowed networks.
func (d *Dialer) checkAllowed(host string) error {
	// If host is already an IP, parse directly
	if ip := net.ParseIP(host); ip != nil {
		if !d.ipAllowed(ip) {
			return fmt.Errorf("dial rejected: %s is not in allowed networks", host)
		}
		return nil
	}

	// Resolve hostname
	ips, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("dial rejected: cannot resolve %s: %w", host, err)
	}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil || !d.ipAllowed(ip) {
			return fmt.Errorf("dial rejected: %s resolved to %s which is not in allowed networks", host, ipStr)
		}
	}
	return nil
}

func (d *Dialer) ipAllowed(ip net.IP) bool {
	for _, n := range d.allowedNetworks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// negotiate performs telnet option negotiation including terminal type and window size
func (d *Dialer) negotiate(conn net.Conn) {
	// Set a write deadline so we don't hang on a non-responsive BBS
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

	// Advertise our capabilities to the remote BBS
	conn.Write([]byte{
		IAC, WILL, SUPPRESS_GO_AHEAD,
		IAC, DO, SUPPRESS_GO_AHEAD,
		IAC, WILL, TERMINAL_TYPE,
		IAC, WILL, NAWS,
	})

	// Send initial NAWS (80x24) immediately so the remote has window size
	// Format: IAC SB NAWS <width-hi> <width-lo> <height-hi> <height-lo> IAC SE
	conn.Write([]byte{
		IAC, SB, NAWS, 0, 80, 0, 24, IAC, SE,
	})

	// Clear the deadline for normal data flow
	conn.SetWriteDeadline(time.Time{})
}

// TelnetFilter filters telnet commands from data stream
type TelnetFilter struct {
	state   filterState
	command byte
	optData []byte

	// Set on filters that face a remote BBS. A telnet peer doubles 0xFF in
	// data; a raw TCP peer does not — and the two are indistinguishable on the
	// wire, so the filter has to learn which kind of peer it is talking to
	// before binary data starts flowing. Client-facing filters leave this off:
	// the web proxy always doubles 0xFF, so their contract is unambiguous.
	learnPeer bool
	// peerNegotiated records that the peer sent well-formed telnet negotiation.
	peerNegotiated bool
	// peerLocked freezes peerNegotiated once a binary transfer begins, so file
	// data that happens to contain IAC WILL can no longer flip the verdict
	// mid-stream.
	peerLocked bool
	// frame holds the last two data bytes, enough to spot a ZMODEM frame header.
	frame [2]byte
}

type filterState int

const (
	stateData filterState = iota
	stateIAC
	stateCommand
	stateSB
	stateSBData
	stateSBIAC
)

// NewTelnetFilter creates a new telnet filter. Use this for client-facing
// input, where 0xFF is always correctly doubled.
func NewTelnetFilter() *TelnetFilter {
	return &TelnetFilter{
		state:   stateData,
		optData: make([]byte, 0, 256),
	}
}

// NewRemoteTelnetFilter creates a filter for the BBS→client direction. Unlike
// the client-facing filter it decides for itself whether the far end speaks
// telnet, and stops treating 0xFF as IAC when it doesn't. Raw TCP BBSes send
// undoubled 0xFF inside ZMODEM file data, and interpreting that as a telnet
// command corrupts the transfer.
func NewRemoteTelnetFilter() *TelnetFilter {
	f := NewTelnetFilter()
	f.learnPeer = true
	return f
}

// PeerSpeaksTelnet reports whether the far end has proved itself a telnet peer
// by answering negotiation. Data written back to such a peer must have its
// 0xFF bytes doubled per RFC 854; a raw TCP peer must receive them undoubled.
func (f *TelnetFilter) PeerSpeaksTelnet() bool {
	return f.peerNegotiated
}

// iacIsData reports whether 0xFF should pass through untouched, which is the
// case once we have concluded the peer is not speaking telnet.
func (f *TelnetFilter) iacIsData() bool {
	return f.learnPeer && f.peerLocked && !f.peerNegotiated
}

// noteDataByte watches the data stream for the start of a ZMODEM frame
// (ZPAD ZDLE ZBIN/ZHEX/ZBIN32) and locks the peer verdict when it sees one.
// ZMODEM escapes ZDLE inside subpackets, so an unescaped 0x18 here is always a
// real frame marker rather than file content.
func (f *TelnetFilter) noteDataByte(b byte) {
	if !f.learnPeer {
		return
	}
	if !f.peerLocked && f.frame[0] == ZPAD && f.frame[1] == ZDLE &&
		(b == ZBIN || b == ZHEX || b == ZBIN32) {
		f.peerLocked = true
	}
	f.frame[0], f.frame[1] = f.frame[1], b
}

// Filter processes bytes and returns filtered data and any responses needed
func (f *TelnetFilter) Filter(data []byte) (filtered []byte, responses []byte) {
	filtered = make([]byte, 0, len(data))
	responses = make([]byte, 0)

	for _, b := range data {
		switch f.state {
		case stateData:
			if b == IAC && !f.iacIsData() {
				f.state = stateIAC
			} else {
				filtered = append(filtered, b)
				f.noteDataByte(b)
			}

		case stateIAC:
			switch b {
			case IAC:
				// Escaped IAC, output single IAC
				filtered = append(filtered, IAC)
				f.noteDataByte(IAC)
				f.state = stateData
			case WILL, WONT, DO, DONT:
				f.command = b
				f.state = stateCommand
			case SB:
				f.state = stateSB
				f.optData = f.optData[:0]
			default:
				if b < SE {
					// Not a telnet command at all (commands are 240-255).
					// A peer that doesn't speak telnet — plenty of BBSes are
					// raw TCP — sends 0xFF as ordinary data without doubling
					// it. Swallowing IAC plus the byte after it corrupts every
					// binary stream that contains 0xFF, which is what breaks
					// ZMODEM transfers. Emit both bytes literally instead.
					filtered = append(filtered, IAC, b)
				}
				// Otherwise a standalone command (GA, NOP, DM, ...): consume it.
				f.state = stateData
			}

		case stateCommand:
			// A complete WILL/WONT/DO/DONT exchange is proof the peer speaks
			// telnet, so its 0xFF bytes really are doubled IAC.
			if !f.peerLocked {
				f.peerNegotiated = true
			}
			// Respond to option requests
			resp := f.respondToOption(f.command, b)
			responses = append(responses, resp...)
			f.state = stateData

		case stateSB:
			// First byte is option code
			f.optData = append(f.optData, b)
			f.state = stateSBData

		case stateSBData:
			if b == IAC {
				f.state = stateSBIAC
			} else if len(f.optData) < 512 {
				f.optData = append(f.optData, b)
			} else {
				// Subnegotiation too large, discard and reset
				f.optData = f.optData[:0]
				f.state = stateData
			}

		case stateSBIAC:
			if b == SE {
				// A completed subnegotiation is equally proof of telnet.
				if !f.peerLocked {
					f.peerNegotiated = true
				}
				// End of subnegotiation - handle the request
				resp := f.handleSubnegotiation()
				responses = append(responses, resp...)
				f.state = stateData
			} else if b == IAC {
				f.optData = append(f.optData, IAC)
				f.state = stateSBData
			} else {
				f.state = stateData
			}
		}
	}

	return filtered, responses
}

// handleSubnegotiation processes a completed subnegotiation sequence.
// optData[0] is the option code, optData[1:] is the payload.
func (f *TelnetFilter) handleSubnegotiation() []byte {
	if len(f.optData) < 2 {
		return nil
	}

	opt := f.optData[0]
	qualifier := f.optData[1]

	switch opt {
	case TERMINAL_TYPE:
		if qualifier == TTYPE_SEND {
			// Remote asked for our terminal type — respond with "ANSI"
			ttype := []byte("ANSI")
			resp := []byte{IAC, SB, TERMINAL_TYPE, TTYPE_IS}
			resp = append(resp, ttype...)
			resp = append(resp, IAC, SE)
			return resp
		}
	}

	return nil
}

func (f *TelnetFilter) respondToOption(cmd, opt byte) []byte {
	switch cmd {
	case DO:
		switch opt {
		case SUPPRESS_GO_AHEAD, TERMINAL_TYPE, NAWS:
			return []byte{IAC, WILL, opt}
		default:
			return []byte{IAC, WONT, opt}
		}
	case WILL:
		switch opt {
		case SUPPRESS_GO_AHEAD, ECHO:
			return []byte{IAC, DO, opt}
		default:
			return []byte{IAC, DONT, opt}
		}
	case DONT, WONT:
		// Acknowledge
		return nil
	}
	return nil
}
