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
	state    filterState
	command  byte
	optData  []byte
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

// NewTelnetFilter creates a new telnet filter
func NewTelnetFilter() *TelnetFilter {
	return &TelnetFilter{
		state:   stateData,
		optData: make([]byte, 0, 256),
	}
}

// Filter processes bytes and returns filtered data and any responses needed
func (f *TelnetFilter) Filter(data []byte) (filtered []byte, responses []byte) {
	filtered = make([]byte, 0, len(data))
	responses = make([]byte, 0)

	for _, b := range data {
		switch f.state {
		case stateData:
			if b == IAC {
				f.state = stateIAC
			} else {
				filtered = append(filtered, b)
			}

		case stateIAC:
			switch b {
			case IAC:
				// Escaped IAC, output single IAC
				filtered = append(filtered, IAC)
				f.state = stateData
			case WILL, WONT, DO, DONT:
				f.command = b
				f.state = stateCommand
			case SB:
				f.state = stateSB
				f.optData = f.optData[:0]
			default:
				// Other commands like GA, NOP, etc.
				f.state = stateData
			}

		case stateCommand:
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
