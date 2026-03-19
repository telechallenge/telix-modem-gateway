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
		{ResultNoAnswer, "NO ANSWER"},
	}

	for _, test := range tests {
		result := test.code.String()
		if result != test.expected {
			t.Errorf("ResultCode(%d).String() = %q, want %q", test.code, result, test.expected)
		}
	}
}
