package dialer

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTelnetFilter(t *testing.T) {
	tests := []struct {
		name         string
		inputs       [][]byte // multiple calls to Filter
		wantFiltered []byte
		wantResponse []byte
	}{
		// 1. Plain data passthrough
		{
			name:         "plain ASCII passes through unchanged",
			inputs:       [][]byte{[]byte("Hello, world!")},
			wantFiltered: []byte("Hello, world!"),
			wantResponse: nil,
		},
		{
			name:         "empty input produces empty output",
			inputs:       [][]byte{{}},
			wantFiltered: []byte{},
			wantResponse: nil,
		},
		{
			name:         "binary data without IAC passes through",
			inputs:       [][]byte{{0x00, 0x01, 0x7F, 0xFE}}, // 0xFE = DONT, but without IAC prefix it's data
			wantFiltered: []byte{0x00, 0x01, 0x7F, 0xFE},
			wantResponse: nil,
		},

		// 2. IAC escaping
		{
			name:         "IAC IAC produces single IAC byte",
			inputs:       [][]byte{{IAC, IAC}},
			wantFiltered: []byte{IAC},
			wantResponse: nil,
		},
		{
			name:         "data around escaped IAC",
			inputs:       [][]byte{{'A', IAC, IAC, 'B'}},
			wantFiltered: []byte{'A', IAC, 'B'},
			wantResponse: nil,
		},
		{
			name:         "multiple escaped IACs",
			inputs:       [][]byte{{IAC, IAC, IAC, IAC}},
			wantFiltered: []byte{IAC, IAC},
			wantResponse: nil,
		},

		// 3. WILL/WONT/DO/DONT handling
		{
			name:         "DO SUPPRESS_GO_AHEAD responds WILL SUPPRESS_GO_AHEAD",
			inputs:       [][]byte{{IAC, DO, SUPPRESS_GO_AHEAD}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, WILL, SUPPRESS_GO_AHEAD},
		},
		{
			name:         "DO TERMINAL_TYPE responds WILL TERMINAL_TYPE",
			inputs:       [][]byte{{IAC, DO, TERMINAL_TYPE}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, WILL, TERMINAL_TYPE},
		},
		{
			name:         "DO NAWS responds WILL NAWS",
			inputs:       [][]byte{{IAC, DO, NAWS}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, WILL, NAWS},
		},
		{
			name:         "DO unknown option responds WONT",
			inputs:       [][]byte{{IAC, DO, 99}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, WONT, 99},
		},
		{
			name:         "WILL SUPPRESS_GO_AHEAD responds DO SUPPRESS_GO_AHEAD",
			inputs:       [][]byte{{IAC, WILL, SUPPRESS_GO_AHEAD}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, DO, SUPPRESS_GO_AHEAD},
		},
		{
			name:         "WILL ECHO responds DO ECHO",
			inputs:       [][]byte{{IAC, WILL, ECHO}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, DO, ECHO},
		},
		{
			name:         "WILL unknown option responds DONT",
			inputs:       [][]byte{{IAC, WILL, 99}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, DONT, 99},
		},
		{
			name:         "DONT produces no response",
			inputs:       [][]byte{{IAC, DONT, SUPPRESS_GO_AHEAD}},
			wantFiltered: nil,
			wantResponse: nil,
		},
		{
			name:         "WONT produces no response",
			inputs:       [][]byte{{IAC, WONT, ECHO}},
			wantFiltered: nil,
			wantResponse: nil,
		},

		// 4. Subnegotiation — terminal type
		{
			name: "SB TERMINAL_TYPE SEND responds with ANSI",
			inputs: [][]byte{{
				IAC, SB, TERMINAL_TYPE, TTYPE_SEND, IAC, SE,
			}},
			wantFiltered: nil,
			wantResponse: []byte{
				IAC, SB, TERMINAL_TYPE, TTYPE_IS,
				'A', 'N', 'S', 'I',
				IAC, SE,
			},
		},

		// 5. Subnegotiation with unknown option — no response
		{
			name: "SB unknown option produces no response",
			inputs: [][]byte{{
				IAC, SB, 99, TTYPE_SEND, IAC, SE,
			}},
			wantFiltered: nil,
			wantResponse: nil,
		},
		{
			name: "SB with only option byte and no qualifier produces no response",
			inputs: [][]byte{{
				IAC, SB, TERMINAL_TYPE, IAC, SE,
			}},
			wantFiltered: nil,
			wantResponse: nil,
		},

		// 6. Mixed data and commands
		{
			name: "data interleaved with telnet commands",
			inputs: [][]byte{{
				'H', 'i',
				IAC, WILL, ECHO,
				'!',
			}},
			wantFiltered: []byte("Hi!"),
			wantResponse: []byte{IAC, DO, ECHO},
		},
		{
			name: "multiple commands mixed with data",
			inputs: [][]byte{{
				'A',
				IAC, DO, SUPPRESS_GO_AHEAD,
				'B',
				IAC, WILL, ECHO,
				'C',
			}},
			wantFiltered: []byte("ABC"),
			wantResponse: []byte{
				IAC, WILL, SUPPRESS_GO_AHEAD,
				IAC, DO, ECHO,
			},
		},
		{
			name: "subnegotiation between data",
			inputs: [][]byte{{
				'X',
				IAC, SB, TERMINAL_TYPE, TTYPE_SEND, IAC, SE,
				'Y',
			}},
			wantFiltered: []byte("XY"),
			wantResponse: []byte{
				IAC, SB, TERMINAL_TYPE, TTYPE_IS,
				'A', 'N', 'S', 'I',
				IAC, SE,
			},
		},

		// 7. Incremental filtering — sequence split across calls
		{
			name: "IAC DO split across two calls",
			inputs: [][]byte{
				{IAC},
				{DO, TERMINAL_TYPE},
			},
			wantFiltered: nil,
			wantResponse: []byte{IAC, WILL, TERMINAL_TYPE},
		},
		{
			name: "IAC split from WILL option",
			inputs: [][]byte{
				{'A', IAC},
				{WILL},
				{ECHO, 'B'},
			},
			wantFiltered: []byte("AB"),
			wantResponse: []byte{IAC, DO, ECHO},
		},
		{
			name: "subnegotiation split across multiple calls",
			inputs: [][]byte{
				{IAC, SB},
				{TERMINAL_TYPE},
				{TTYPE_SEND},
				{IAC},
				{SE},
			},
			wantFiltered: nil,
			wantResponse: []byte{
				IAC, SB, TERMINAL_TYPE, TTYPE_IS,
				'A', 'N', 'S', 'I',
				IAC, SE,
			},
		},
		{
			name: "IAC IAC split across two calls",
			inputs: [][]byte{
				{'A', IAC},
				{IAC, 'B'},
			},
			wantFiltered: []byte{'A', IAC, 'B'},
			wantResponse: nil,
		},

		// 8. Oversized subnegotiation resets to data state
		{
			name: "subnegotiation exceeding 512 bytes resets to data state",
			inputs: func() [][]byte {
				// IAC SB <option> <512 bytes of data> <next byte triggers reset>
				// After the option byte, we need 512 more bytes in stateSBData
				// to hit the overflow. The option byte itself is appended in stateSB,
				// so we need 511 more in stateSBData before the 512th triggers reset.
				buf := make([]byte, 0, 520)
				buf = append(buf, IAC, SB, TERMINAL_TYPE)
				// The option byte (TERMINAL_TYPE) is byte 0 in optData (appended in stateSB).
				// Now we're in stateSBData. We need to add 511 bytes to reach len(optData)==512,
				// then the next byte will trigger the reset.
				for i := 0; i < 511; i++ {
					buf = append(buf, 'X')
				}
				// This byte should trigger the overflow (len(optData) == 512)
				buf = append(buf, 'Z')
				// After reset to stateData, normal data should pass through
				buf = append(buf, 'A', 'B')
				return [][]byte{buf}
			}(),
			wantFiltered: []byte("AB"),
			wantResponse: nil,
		},

		// 9. IAC IAC within subnegotiation is unescaped
		{
			name: "IAC IAC in subnegotiation data is unescaped to single IAC",
			inputs: [][]byte{{
				IAC, SB, TERMINAL_TYPE, TTYPE_SEND,
				IAC, IAC, // escaped IAC within SB data
				IAC, SE,
			}},
			// The TTYPE_SEND qualifier triggers the terminal type response
			// regardless of extra data after it. The IAC is appended to optData.
			wantFiltered: nil,
			wantResponse: []byte{
				IAC, SB, TERMINAL_TYPE, TTYPE_IS,
				'A', 'N', 'S', 'I',
				IAC, SE,
			},
		},

		// Additional edge cases
		{
			name:         "unknown IAC command (e.g. GA=249) is consumed silently",
			inputs:       [][]byte{{'A', IAC, 249, 'B'}},
			wantFiltered: []byte("AB"),
			wantResponse: nil,
		},

		// 8. Raw (non-telnet) peers send 0xFF undoubled. Byte values below SE
		// are not telnet commands, so IAC plus that byte must reach the client
		// intact — dropping the pair corrupts binary transfers like ZMODEM.
		{
			name:         "IAC followed by a non-command byte passes both through",
			inputs:       [][]byte{{IAC, 0x06}},
			wantFiltered: []byte{IAC, 0x06},
			wantResponse: nil,
		},
		{
			name:         "undoubled 0xFF inside binary data survives with neighbours",
			inputs:       [][]byte{{0xF8, IAC, 0x06, 0x0D}},
			wantFiltered: []byte{0xF8, IAC, 0x06, 0x0D},
			wantResponse: nil,
		},
		{
			name:         "IAC before NUL passes through",
			inputs:       [][]byte{{IAC, 0x00, 'Z'}},
			wantFiltered: []byte{IAC, 0x00, 'Z'},
			wantResponse: nil,
		},
		{
			name:         "IAC then non-command split across Filter calls",
			inputs:       [][]byte{{'A', IAC}, {0x06, 'B'}},
			wantFiltered: []byte{'A', IAC, 0x06, 'B'},
			wantResponse: nil,
		},
		{
			name:         "SE=240 alone is still treated as a command, not data",
			inputs:       [][]byte{{'A', IAC, SE, 'B'}},
			wantFiltered: []byte("AB"),
			wantResponse: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewTelnetFilter()

			var allFiltered []byte
			var allResponses []byte

			for _, input := range tt.inputs {
				filtered, responses := f.Filter(input)
				allFiltered = append(allFiltered, filtered...)
				allResponses = append(allResponses, responses...)
			}

			// Normalize nil vs empty for comparison
			if tt.wantFiltered == nil {
				tt.wantFiltered = []byte{}
			}
			if tt.wantResponse == nil {
				tt.wantResponse = []byte{}
			}

			if !bytes.Equal(allFiltered, tt.wantFiltered) {
				t.Errorf("filtered:\n  got  %v\n  want %v", allFiltered, tt.wantFiltered)
			}
			if !bytes.Equal(allResponses, tt.wantResponse) {
				t.Errorf("responses:\n  got  %v\n  want %v", allResponses, tt.wantResponse)
			}
		})
	}
}

// --- Network restriction tests ---

func mustParseCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("invalid CIDR %q: %v", cidr, err)
	}
	return ipNet
}

// A raw TCP BBS never negotiates and never doubles 0xFF. Once a ZMODEM frame
// starts, its file data must reach the client byte for byte — the bug this
// guards against silently ate 0xFF and the byte after it, corrupting downloads.
func TestRemoteTelnetFilter_RawPeerPassesBinaryThrough(t *testing.T) {
	zmodemStart := []byte{ZPAD, ZPAD, ZDLE, ZHEX, '0', '0'}

	tests := []struct {
		name string
		data []byte // sent after the ZMODEM frame header
		want []byte
	}{
		{
			name: "0xFF followed by a telnet command byte survives",
			data: []byte{0x01, IAC, WILL, 0x02},
			want: []byte{0x01, IAC, WILL, 0x02},
		},
		{
			name: "doubled-looking 0xFF stays two bytes for a raw peer",
			data: []byte{IAC, IAC},
			want: []byte{IAC, IAC},
		},
		{
			name: "0xFF before SE survives",
			data: []byte{IAC, SE, 'x'},
			want: []byte{IAC, SE, 'x'},
		},
		{
			name: "0xFF before a subnegotiation byte survives",
			data: []byte{IAC, SB, NAWS},
			want: []byte{IAC, SB, NAWS},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewRemoteTelnetFilter()
			f.Filter(zmodemStart)
			got, responses := f.Filter(tt.data)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("filtered = % 02x, want % 02x", got, tt.want)
			}
			if len(responses) != 0 {
				t.Errorf("raw peer must not draw telnet responses, got % 02x", responses)
			}
		})
	}
}

// The mirror case: a BBS that did negotiate is a real telnet peer, so its
// doubled 0xFF must still collapse to one byte even during a transfer.
func TestRemoteTelnetFilter_TelnetPeerStillUnescapes(t *testing.T) {
	f := NewRemoteTelnetFilter()

	// Login-time negotiation proves the peer speaks telnet.
	if _, responses := f.Filter([]byte{IAC, DO, SUPPRESS_GO_AHEAD}); len(responses) == 0 {
		t.Fatal("expected a negotiation response")
	}
	f.Filter([]byte{ZPAD, ZPAD, ZDLE, ZHEX}) // transfer begins

	got, _ := f.Filter([]byte{'a', IAC, IAC, 'b'})
	want := []byte{'a', IAC, 'b'}
	if !bytes.Equal(got, want) {
		t.Errorf("filtered = % 02x, want % 02x", got, want)
	}
}

// Once a transfer is under way the verdict must not change, or file data that
// happens to contain IAC WILL would flip a raw peer into telnet mode mid-stream.
func TestRemoteTelnetFilter_VerdictLocksAtTransferStart(t *testing.T) {
	f := NewRemoteTelnetFilter()
	f.Filter([]byte{ZPAD, ZPAD, ZDLE, ZBIN32}) // transfer begins, peer never negotiated

	// This looks exactly like negotiation but is file content.
	if _, responses := f.Filter([]byte{IAC, DO, SUPPRESS_GO_AHEAD}); len(responses) != 0 {
		t.Errorf("locked raw peer must not answer negotiation, got % 02x", responses)
	}
	got, _ := f.Filter([]byte{IAC, IAC})
	if want := []byte{IAC, IAC}; !bytes.Equal(got, want) {
		t.Errorf("after lock filtered = % 02x, want % 02x", got, want)
	}
}

// Client-facing input keeps the strict contract: the web proxy always doubles
// 0xFF, so the filter must always undo it regardless of ZMODEM traffic.
func TestClientTelnetFilter_AlwaysUnescapesIAC(t *testing.T) {
	f := NewTelnetFilter()
	f.Filter([]byte{ZPAD, ZPAD, ZDLE, ZHEX}) // an upload begins

	got, _ := f.Filter([]byte{IAC, IAC})
	if want := []byte{IAC}; !bytes.Equal(got, want) {
		t.Errorf("filtered = % 02x, want % 02x", got, want)
	}
}

func TestDialerIPAllowed_WithinCIDR(t *testing.T) {
	d := New(5*time.Second, []*net.IPNet{
		mustParseCIDR(t, "10.0.0.0/8"),
	})
	ip := net.ParseIP("10.1.2.3")
	if !d.ipAllowed(ip) {
		t.Errorf("ipAllowed(%s) = false, want true for CIDR 10.0.0.0/8", ip)
	}
}

func TestDialerIPAllowed_OutsideCIDR(t *testing.T) {
	d := New(5*time.Second, []*net.IPNet{
		mustParseCIDR(t, "10.0.0.0/8"),
	})
	ip := net.ParseIP("192.168.1.1")
	if d.ipAllowed(ip) {
		t.Errorf("ipAllowed(%s) = true, want false for CIDR 10.0.0.0/8", ip)
	}
}

func TestDialerIPAllowed_MultipleCIDRs(t *testing.T) {
	d := New(5*time.Second, []*net.IPNet{
		mustParseCIDR(t, "10.0.0.0/8"),
		mustParseCIDR(t, "172.16.0.0/12"),
		mustParseCIDR(t, "192.168.0.0/16"),
	})

	tests := []struct {
		ip   string
		want bool
	}{
		{"10.255.0.1", true},
		{"172.20.5.5", true},
		{"192.168.99.1", true},
		{"8.8.8.8", false},
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		got := d.ipAllowed(ip)
		if got != tt.want {
			t.Errorf("ipAllowed(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestDialerIPAllowed_EmptyNetworks(t *testing.T) {
	d := New(5*time.Second, nil)
	ip := net.ParseIP("10.0.0.1")
	if d.ipAllowed(ip) {
		t.Errorf("ipAllowed(%s) with empty allowedNetworks = true, want false", ip)
	}
}

func TestDialerCheckAllowed_IPAddress(t *testing.T) {
	d := New(5*time.Second, []*net.IPNet{
		mustParseCIDR(t, "192.168.0.0/16"),
	})
	err := d.checkAllowed("192.168.1.100")
	if err != nil {
		t.Errorf("checkAllowed(192.168.1.100) returned error: %v", err)
	}
}

func TestDialerCheckAllowed_IPAddress_Rejected(t *testing.T) {
	d := New(5*time.Second, []*net.IPNet{
		mustParseCIDR(t, "192.168.0.0/16"),
	})
	err := d.checkAllowed("10.0.0.1")
	if err == nil {
		t.Error("checkAllowed(10.0.0.1) should fail when only 192.168.0.0/16 is allowed")
	}
	if !strings.Contains(err.Error(), "not in allowed networks") {
		t.Errorf("error message should mention 'not in allowed networks', got: %v", err)
	}
}

func TestDialerCheckAllowed_Hostname_Localhost(t *testing.T) {
	// localhost may resolve to both 127.0.0.1 and ::1, so include both
	// IPv4 and IPv6 loopback CIDRs. checkAllowed rejects if ANY resolved
	// IP is outside the allowed set.
	d := New(5*time.Second, []*net.IPNet{
		mustParseCIDR(t, "127.0.0.0/8"),
		mustParseCIDR(t, "::1/128"),
	})
	err := d.checkAllowed("localhost")
	if err != nil {
		t.Errorf("checkAllowed(localhost) with loopback CIDRs allowed returned error: %v", err)
	}
}

func TestDialerDial_RejectedByNetwork(t *testing.T) {
	// Only allow 10.0.0.0/8 — dialing 127.0.0.1 should be rejected
	// without establishing a TCP connection.
	d := New(5*time.Second, []*net.IPNet{
		mustParseCIDR(t, "10.0.0.0/8"),
	})

	conn, err := d.Dial("127.0.0.1", 9999)
	if conn != nil {
		conn.Close()
		t.Error("Dial should not return a connection when IP is outside allowed networks")
	}
	if err == nil {
		t.Fatal("Dial should return an error when IP is outside allowed networks")
	}
	if !strings.Contains(err.Error(), "not in allowed networks") {
		t.Errorf("error should mention 'not in allowed networks', got: %v", err)
	}
}
