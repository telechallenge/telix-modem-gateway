package modem

import "testing"

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input    string
		expected CommandType
	}{
		{"AT", CmdAT},
		{"at", CmdAT},
		{"ATZ", CmdReset},
		{"atz", CmdReset},
		{"AT&F", CmdFactoryReset},
		{"ATH", CmdHangup},
		{"ATH0", CmdHangup},
		{"ATH1", CmdOffHook},
		{"ATE0", CmdEchoOff},
		{"ATE1", CmdEchoOn},
		{"ATV0", CmdVerboseOff},
		{"ATV1", CmdVerboseOn},
		{"ATQ0", CmdQuietOff},
		{"ATQ1", CmdQuietOn},
		{"ATI", CmdIdentify},
		{"AT&V", CmdViewConfig},
		{"ATO", CmdOnline},
		{"ATO0", CmdOnline},
		{"ATCLS", CmdClear},
		{"atcls", CmdClear},
		{"ATDT916-555-1212", CmdDial},
		{"ATDP415-555-0100", CmdDial},
		{"ATS0=1", CmdSRegisterSet},
		{"ATS7?", CmdSRegisterQuery},
		{"ATXYZ", CmdUnknown},
		{"HELLO", CmdUnknown},
	}

	for _, test := range tests {
		cmd := ParseCommand(test.input)
		if cmd.Type != test.expected {
			t.Errorf("ParseCommand(%q) = %v, want %v", test.input, cmd.Type, test.expected)
		}
	}
}

func TestParseDialCommand(t *testing.T) {
	cmd := ParseCommand("ATDT916-555-1212")
	if cmd.Type != CmdDial {
		t.Errorf("Expected CmdDial, got %v", cmd.Type)
	}
	if cmd.Number != "916-555-1212" {
		t.Errorf("Expected number 916-555-1212, got %s", cmd.Number)
	}
}

func TestParseSRegisterSet(t *testing.T) {
	cmd := ParseCommand("ATS7=30")
	if cmd.Type != CmdSRegisterSet {
		t.Errorf("Expected CmdSRegisterSet, got %v", cmd.Type)
	}
	if cmd.Register != 7 {
		t.Errorf("Expected register 7, got %d", cmd.Register)
	}
	if cmd.Value != 30 {
		t.Errorf("Expected value 30, got %d", cmd.Value)
	}
}

func TestParseSRegisterQuery(t *testing.T) {
	cmd := ParseCommand("ATS12?")
	if cmd.Type != CmdSRegisterQuery {
		t.Errorf("Expected CmdSRegisterQuery, got %v", cmd.Type)
	}
	if cmd.Register != 12 {
		t.Errorf("Expected register 12, got %d", cmd.Register)
	}
}

func TestParseCommandLine(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []CommandType
	}{
		{"single AT", "AT", []CommandType{CmdAT}},
		{"single reset", "ATZ", []CommandType{CmdReset}},
		{"single verbose", "ATV1", []CommandType{CmdVerboseOn}},
		{"compound with spaces", "AT V1 X4 S11=30 S7=60", []CommandType{CmdVerboseOn, CmdSetX, CmdSRegisterSet, CmdSRegisterSet}},
		{"compound no spaces", "ATV1X4S11=30S7=60", []CommandType{CmdVerboseOn, CmdSetX, CmdSRegisterSet, CmdSRegisterSet}},
		{"compound mixed", "ATE0 V1X4", []CommandType{CmdEchoOff, CmdVerboseOn, CmdSetX}},
		{"dial consumes rest", "ATE0V1DT916-555-1212", []CommandType{CmdEchoOff, CmdVerboseOn, CmdDial}},
		{"ampersand commands", "AT&Q5%C1V1", []CommandType{CmdSetErrorCorrection, CmdSetCompression, CmdVerboseOn}},
		{"s-register query", "ATV1S12?", []CommandType{CmdVerboseOn, CmdSRegisterQuery}},
		{"factory reset", "AT&F", []CommandType{CmdFactoryReset}},
		{"atcls", "ATCLS", []CommandType{CmdClear}},
		{"lock speed", "AT&N8V1", []CommandType{CmdLockSpeed, CmdVerboseOn}},
		{"not AT prefix", "HELLO", []CommandType{CmdUnknown}},
		{"unknown sub-command stops", "ATJV1", []CommandType{CmdUnknown}},
		{"hangup variants", "ATH0V1", []CommandType{CmdHangup, CmdVerboseOn}},
		{"online return", "ATO", []CommandType{CmdOnline}},
		{"identify", "ATI", []CommandType{CmdIdentify}},
		{"case insensitive", "at v1 x4 s11=30", []CommandType{CmdVerboseOn, CmdSetX, CmdSRegisterSet}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmds := ParseCommandLine(tt.input)
			if len(cmds) != len(tt.expected) {
				t.Fatalf("ParseCommandLine(%q) returned %d commands, want %d", tt.input, len(cmds), len(tt.expected))
			}
			for i, cmd := range cmds {
				if cmd.Type != tt.expected[i] {
					t.Errorf("ParseCommandLine(%q)[%d].Type = %v, want %v", tt.input, i, cmd.Type, tt.expected[i])
				}
			}
		})
	}
}

func TestParseCommandLineValues(t *testing.T) {
	// Verify values are correctly parsed for compound commands
	cmds := ParseCommandLine("AT X4 S11=30 S7=60")
	if len(cmds) != 3 {
		t.Fatalf("expected 3 commands, got %d", len(cmds))
	}
	if cmds[0].Value != 4 {
		t.Errorf("X4 value = %d, want 4", cmds[0].Value)
	}
	if cmds[1].Register != 11 || cmds[1].Value != 30 {
		t.Errorf("S11=30 got register=%d value=%d", cmds[1].Register, cmds[1].Value)
	}
	if cmds[2].Register != 7 || cmds[2].Value != 60 {
		t.Errorf("S7=60 got register=%d value=%d", cmds[2].Register, cmds[2].Value)
	}

	// Verify dial number preserved
	cmds = ParseCommandLine("ATE0DT916-555-1212")
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(cmds))
	}
	if cmds[1].Number != "916-555-1212" {
		t.Errorf("dial number = %q, want %q", cmds[1].Number, "916-555-1212")
	}
}

func TestResultCodeString(t *testing.T) {
	tests := []struct {
		code     ResultCode
		expected string
	}{
		{ResultOK, "OK"},
		{ResultConnect, "CONNECT"},
		{ResultRing, "RING"},
		{ResultNoCarrier, "NO CARRIER"},
		{ResultError, "ERROR"},
		{ResultBusy, "BUSY"},
		{ResultNoAnswer, "NO ANSWER"},
	}

	for _, test := range tests {
		result := test.code.String()
		if result != test.expected {
			t.Errorf("ResultCode(%d).String() = %q, want %q", test.code, result, test.expected)
		}
	}
}
