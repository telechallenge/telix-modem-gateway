package dialer

import (
	"bytes"
	"testing"
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
			name:   "DO SUPPRESS_GO_AHEAD responds WILL SUPPRESS_GO_AHEAD",
			inputs: [][]byte{{IAC, DO, SUPPRESS_GO_AHEAD}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, WILL, SUPPRESS_GO_AHEAD},
		},
		{
			name:   "DO TERMINAL_TYPE responds WILL TERMINAL_TYPE",
			inputs: [][]byte{{IAC, DO, TERMINAL_TYPE}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, WILL, TERMINAL_TYPE},
		},
		{
			name:   "DO NAWS responds WILL NAWS",
			inputs: [][]byte{{IAC, DO, NAWS}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, WILL, NAWS},
		},
		{
			name:   "DO unknown option responds WONT",
			inputs: [][]byte{{IAC, DO, 99}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, WONT, 99},
		},
		{
			name:   "WILL SUPPRESS_GO_AHEAD responds DO SUPPRESS_GO_AHEAD",
			inputs: [][]byte{{IAC, WILL, SUPPRESS_GO_AHEAD}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, DO, SUPPRESS_GO_AHEAD},
		},
		{
			name:   "WILL ECHO responds DO ECHO",
			inputs: [][]byte{{IAC, WILL, ECHO}},
			wantFiltered: nil,
			wantResponse: []byte{IAC, DO, ECHO},
		},
		{
			name:   "WILL unknown option responds DONT",
			inputs: [][]byte{{IAC, WILL, 99}},
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
