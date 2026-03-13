package modem

import (
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// State methods
// ---------------------------------------------------------------------------

func TestNew_Defaults(t *testing.T) {
	m := New("test")

	if m.Echo() != true {
		t.Error("expected echo=true")
	}
	if m.Verbose() != true {
		t.Error("expected verbose=true")
	}
	if m.Quiet() != false {
		t.Error("expected quiet=false")
	}
	if m.GetBaud() != 0 {
		t.Errorf("expected baud=0, got %d", m.GetBaud())
	}
	if m.GetErrorCorrection() != 5 {
		t.Errorf("expected errorCorrection=5, got %d", m.GetErrorCorrection())
	}
	if m.GetCompression() != true {
		t.Error("expected compression=true")
	}
	if m.State() != StateCommand {
		t.Errorf("expected state=StateCommand, got %d", m.State())
	}
	// xlevel is private; verify indirectly through viewConfig containing X4
	resp, _, _ := m.Execute(ParseCommand("AT&V"))
	if !strings.Contains(resp, "X4") {
		t.Errorf("expected viewConfig to contain X4 for default xlevel, got: %s", resp)
	}
}

func TestState_SetState_RoundTrip(t *testing.T) {
	m := New("test")

	if m.State() != StateCommand {
		t.Fatal("initial state should be StateCommand")
	}
	m.SetState(StateData)
	if m.State() != StateData {
		t.Error("expected StateData after SetState")
	}
	m.SetState(StateCommand)
	if m.State() != StateCommand {
		t.Error("expected StateCommand after second SetState")
	}
}

func TestEcho_Verbose_Quiet_Defaults(t *testing.T) {
	m := New("test")

	if !m.Echo() {
		t.Error("Echo should be true by default")
	}
	if !m.Verbose() {
		t.Error("Verbose should be true by default")
	}
	if m.Quiet() {
		t.Error("Quiet should be false by default")
	}
}

func TestGetBaud_GetErrorCorrection_GetCompression_Defaults(t *testing.T) {
	m := New("test")

	if m.GetBaud() != 0 {
		t.Errorf("expected baud=0, got %d", m.GetBaud())
	}
	if m.GetErrorCorrection() != 5 {
		t.Errorf("expected errorCorrection=5, got %d", m.GetErrorCorrection())
	}
	if !m.GetCompression() {
		t.Error("expected compression=true")
	}
}

func TestHasSentInit_ReturnsFalse_ForUnsent(t *testing.T) {
	m := New("test")
	if m.HasSentInit("ATZ") {
		t.Error("HasSentInit should be false for unsent command")
	}
	if m.HasSentInit("AT&F") {
		t.Error("HasSentInit should be false for unsent AT&F")
	}
}

// ---------------------------------------------------------------------------
// Execute() — table-driven tests for every command type
// ---------------------------------------------------------------------------

func TestExecute_AllCommands(t *testing.T) {
	tests := []struct {
		name       string
		rawCmd     string
		wantCode   ResultCode
		wantDial   bool
		checkResp  func(t *testing.T, resp string)
		checkState func(t *testing.T, m *Modem)
	}{
		{
			name:     "CmdAT returns OK",
			rawCmd:   "AT",
			wantCode: ResultOK,
			wantDial: false,
		},
		{
			name:     "CmdReset resets defaults and returns OK",
			rawCmd:   "ATZ",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if !m.Echo() {
					t.Error("expected echo=true after reset")
				}
				if !m.Verbose() {
					t.Error("expected verbose=true after reset")
				}
				if m.Quiet() {
					t.Error("expected quiet=false after reset")
				}
				if m.GetBaud() != 0 {
					t.Errorf("expected baud=0 after reset, got %d", m.GetBaud())
				}
				if !m.HasSentInit("ATZ") {
					t.Error("ATZ should be tracked in initSent")
				}
			},
		},
		{
			name:     "CmdFactoryReset resets defaults and clears initSent",
			rawCmd:   "AT&F",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if !m.Echo() {
					t.Error("expected echo=true after factory reset")
				}
				if !m.Verbose() {
					t.Error("expected verbose=true after factory reset")
				}
				if !m.HasSentInit("AT&F") {
					t.Error("AT&F should be tracked in initSent after factory reset")
				}
			},
		},
		{
			name:     "CmdHangup sets state to StateCommand",
			rawCmd:   "ATH",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if m.State() != StateCommand {
					t.Errorf("expected StateCommand after hangup, got %d", m.State())
				}
			},
		},
		{
			name:     "CmdOffHook returns OK",
			rawCmd:   "ATH1",
			wantCode: ResultOK,
			wantDial: false,
		},
		{
			name:     "CmdEchoOff sets echo false",
			rawCmd:   "ATE0",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if m.Echo() {
					t.Error("expected echo=false after ATE0")
				}
			},
		},
		{
			name:     "CmdEchoOn sets echo true",
			rawCmd:   "ATE1",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if !m.Echo() {
					t.Error("expected echo=true after ATE1")
				}
			},
		},
		{
			name:     "CmdVerboseOff sets verbose false",
			rawCmd:   "ATV0",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if m.Verbose() {
					t.Error("expected verbose=false after ATV0")
				}
			},
		},
		{
			name:     "CmdVerboseOn sets verbose true",
			rawCmd:   "ATV1",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if !m.Verbose() {
					t.Error("expected verbose=true after ATV1")
				}
			},
		},
		{
			name:     "CmdQuietOff sets quiet false",
			rawCmd:   "ATQ0",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if m.Quiet() {
					t.Error("expected quiet=false after ATQ0")
				}
			},
		},
		{
			name:     "CmdQuietOn sets quiet true",
			rawCmd:   "ATQ1",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if !m.Quiet() {
					t.Error("expected quiet=true after ATQ1")
				}
			},
		},
		{
			name:     "CmdIdentify contains version and Hayes Compatible",
			rawCmd:   "ATI",
			wantCode: ResultOK,
			wantDial: false,
			checkResp: func(t *testing.T, resp string) {
				if !strings.Contains(resp, "test") {
					t.Errorf("expected response to contain version string 'test', got: %s", resp)
				}
				if !strings.Contains(resp, "Hayes Compatible") {
					t.Errorf("expected response to contain 'Hayes Compatible', got: %s", resp)
				}
			},
		},
		{
			name:     "CmdViewConfig contains ACTIVE PROFILE and register values",
			rawCmd:   "AT&V",
			wantCode: ResultOK,
			wantDial: false,
			checkResp: func(t *testing.T, resp string) {
				if !strings.Contains(resp, "ACTIVE PROFILE") {
					t.Errorf("expected 'ACTIVE PROFILE' in response, got: %s", resp)
				}
				if !strings.Contains(resp, "S-REGISTERS") {
					t.Errorf("expected 'S-REGISTERS' in response, got: %s", resp)
				}
				if !strings.Contains(resp, "S03=") {
					t.Errorf("expected register S03 in response, got: %s", resp)
				}
			},
		},
		{
			name:     "CmdSRegisterQuery returns formatted register value",
			rawCmd:   "ATS3?",
			wantCode: ResultOK,
			wantDial: false,
			checkResp: func(t *testing.T, resp string) {
				// S3 default is 13 (CR)
				if !strings.Contains(resp, "013") {
					t.Errorf("expected S3 value 013, got: %s", resp)
				}
			},
		},
		{
			name:     "CmdSRegisterSet sets register and returns OK",
			rawCmd:   "ATS0=5",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if m.Registers().Get(0) != 5 {
					t.Errorf("expected S0=5, got %d", m.Registers().Get(0))
				}
			},
		},
		{
			name:     "CmdSetX sets xlevel 0",
			rawCmd:   "ATX0",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				resp, _, _ := m.Execute(ParseCommand("AT&V"))
				if !strings.Contains(resp, "X0") {
					t.Errorf("expected X0 in viewConfig after ATX0, got: %s", resp)
				}
			},
		},
		{
			name:     "CmdSetX sets xlevel 2",
			rawCmd:   "ATX2",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				resp, _, _ := m.Execute(ParseCommand("AT&V"))
				if !strings.Contains(resp, "X2") {
					t.Errorf("expected X2 in viewConfig after ATX2, got: %s", resp)
				}
			},
		},
		{
			name:     "CmdLockSpeed sets baud via SpeedCodeMap",
			rawCmd:   "AT&N3",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if m.GetBaud() != 2400 {
					t.Errorf("expected baud=2400 for &N3, got %d", m.GetBaud())
				}
			},
		},
		{
			name:     "CmdSetErrorCorrection sets errorCorrection",
			rawCmd:   "AT&Q0",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if m.GetErrorCorrection() != 0 {
					t.Errorf("expected errorCorrection=0, got %d", m.GetErrorCorrection())
				}
			},
		},
		{
			name:     "CmdSetCompression disables compression",
			rawCmd:   "AT%C0",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if m.GetCompression() {
					t.Error("expected compression=false after AT%%C0")
				}
			},
		},
		{
			name:     "CmdSetCompression enables compression",
			rawCmd:   "AT%C1",
			wantCode: ResultOK,
			wantDial: false,
			checkState: func(t *testing.T, m *Modem) {
				if !m.GetCompression() {
					t.Error("expected compression=true after AT%%C1")
				}
			},
		},
		{
			name:     "CmdDial returns empty string and dial=true",
			rawCmd:   "ATDT5551234",
			wantCode: ResultOK,
			wantDial: true,
			checkResp: func(t *testing.T, resp string) {
				if resp != "" {
					t.Errorf("expected empty response for dial, got: %q", resp)
				}
			},
		},
		{
			name:     "Unknown command returns ERROR",
			rawCmd:   "ATXYZ999",
			wantCode: ResultError,
			wantDial: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New("test")
			cmd := ParseCommand(tt.rawCmd)
			resp, code, dial := m.Execute(cmd)

			if code != tt.wantCode {
				t.Errorf("Execute(%q): code=%d, want %d", tt.rawCmd, code, tt.wantCode)
			}
			if dial != tt.wantDial {
				t.Errorf("Execute(%q): dial=%v, want %v", tt.rawCmd, dial, tt.wantDial)
			}
			if tt.checkResp != nil {
				tt.checkResp(t, resp)
			}
			if tt.checkState != nil {
				tt.checkState(t, m)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// FormatResultExternal()
// ---------------------------------------------------------------------------

func TestFormatResultExternal_Verbose(t *testing.T) {
	m := New("test")
	// Default: verbose=true, quiet=false
	got := m.FormatResultExternal(ResultOK)
	want := "\r\nOK\r\n"
	if got != want {
		t.Errorf("FormatResultExternal verbose: got %q, want %q", got, want)
	}
}

func TestFormatResultExternal_NonVerbose(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("ATV0"))
	got := m.FormatResultExternal(ResultOK)
	want := "0\r"
	if got != want {
		t.Errorf("FormatResultExternal non-verbose: got %q, want %q", got, want)
	}
}

func TestFormatResultExternal_Quiet(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("ATQ1"))
	got := m.FormatResultExternal(ResultOK)
	if got != "" {
		t.Errorf("FormatResultExternal quiet: got %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// FormatConnectResult()
// ---------------------------------------------------------------------------

func TestFormatConnectResult_X0_BareConnect(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("ATX0"))
	got := m.FormatConnectResult(9600)
	want := "\r\nCONNECT\r\n"
	if got != want {
		t.Errorf("FormatConnectResult X0: got %q, want %q", got, want)
	}
}

func TestFormatConnectResult_X1_WithSpeed(t *testing.T) {
	m := New("test")
	// X4 is default, which is >= 1
	// Disable error correction to test plain speed output
	m.Execute(ParseCommand("AT&Q0"))
	got := m.FormatConnectResult(9600)
	want := "\r\nCONNECT 9600\r\n"
	if got != want {
		t.Errorf("FormatConnectResult X1+ no correction: got %q, want %q", got, want)
	}
}

func TestFormatConnectResult_X1_ErrorCorrection_HighSpeed(t *testing.T) {
	m := New("test")
	// Default: xlevel=4, errorCorrection=5, compression=true
	// Disable compression to test error correction alone
	m.Execute(ParseCommand("AT%C0"))
	got := m.FormatConnectResult(9600)
	want := "\r\nCONNECT 9600/ARQ/V42/LAPM\r\n"
	if got != want {
		t.Errorf("FormatConnectResult X1+ correction: got %q, want %q", got, want)
	}
}

func TestFormatConnectResult_X1_ErrorCorrection_Compression_HighSpeed(t *testing.T) {
	m := New("test")
	// Default: xlevel=4, errorCorrection=5, compression=true
	got := m.FormatConnectResult(9600)
	want := "\r\nCONNECT 9600/ARQ/V42/LAPM/V42BIS\r\n"
	if got != want {
		t.Errorf("FormatConnectResult X1+ correction+compression: got %q, want %q", got, want)
	}
}

func TestFormatConnectResult_X1_ErrorCorrection_LowSpeed(t *testing.T) {
	m := New("test")
	// Default: xlevel=4, errorCorrection=5
	// Speed < 2400 should not show protocol suffix
	got := m.FormatConnectResult(1200)
	want := "\r\nCONNECT 1200\r\n"
	if got != want {
		t.Errorf("FormatConnectResult X1+ low speed: got %q, want %q", got, want)
	}
}

func TestFormatConnectResult_Quiet(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("ATQ1"))
	got := m.FormatConnectResult(9600)
	if got != "" {
		t.Errorf("FormatConnectResult quiet: got %q, want empty", got)
	}
}

func TestFormatConnectResult_NonVerbose(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("ATV0"))
	got := m.FormatConnectResult(9600)
	want := fmt.Sprintf("%d\r", ResultConnect)
	if got != want {
		t.Errorf("FormatConnectResult non-verbose: got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// viewConfig (via CmdViewConfig)
// ---------------------------------------------------------------------------

func TestViewConfig_DefaultValues(t *testing.T) {
	m := New("test")
	resp, _, _ := m.Execute(ParseCommand("AT&V"))

	checks := []string{"E1", "V1", "Q0", "X4"}
	for _, want := range checks {
		if !strings.Contains(resp, want) {
			t.Errorf("viewConfig default: expected %q in response, got: %s", want, resp)
		}
	}
}

func TestViewConfig_AfterChanges(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("ATE0"))
	m.Execute(ParseCommand("ATV0"))
	m.Execute(ParseCommand("ATQ1"))
	m.Execute(ParseCommand("ATX2"))

	resp, _, _ := m.Execute(ParseCommand("AT&V"))

	checks := []string{"E0", "V0", "Q1", "X2"}
	for _, want := range checks {
		if !strings.Contains(resp, want) {
			t.Errorf("viewConfig changed: expected %q in response, got: %s", want, resp)
		}
	}
}

func TestViewConfig_BaudDisplay(t *testing.T) {
	m := New("test")
	// Default baud=0 should show "auto"
	resp, _, _ := m.Execute(ParseCommand("AT&V"))
	if !strings.Contains(resp, "&N=auto") {
		t.Errorf("expected &N=auto for default baud, got: %s", resp)
	}

	// After setting baud, should show numeric value
	m.Execute(ParseCommand("AT&N3"))
	resp, _, _ = m.Execute(ParseCommand("AT&V"))
	if !strings.Contains(resp, "&N=2400") {
		t.Errorf("expected &N=2400 after AT&N3, got: %s", resp)
	}
}

// ---------------------------------------------------------------------------
// Reset behavior
// ---------------------------------------------------------------------------

func TestReset_ATZ_Preserves_InitSent(t *testing.T) {
	m := New("test")
	// Change settings away from defaults
	m.Execute(ParseCommand("ATE0"))
	m.Execute(ParseCommand("ATV0"))
	m.Execute(ParseCommand("ATX2"))
	m.Execute(ParseCommand("AT&N3"))

	// Reset
	m.Execute(ParseCommand("ATZ"))

	// Verify settings are back to defaults
	if !m.Echo() {
		t.Error("expected echo=true after ATZ")
	}
	if !m.Verbose() {
		t.Error("expected verbose=true after ATZ")
	}
	if m.GetBaud() != 0 {
		t.Errorf("expected baud=0 after ATZ, got %d", m.GetBaud())
	}

	// Verify xlevel reset
	resp, _, _ := m.Execute(ParseCommand("AT&V"))
	if !strings.Contains(resp, "X4") {
		t.Errorf("expected xlevel reset to X4 after ATZ, got: %s", resp)
	}

	// Verify initSent is preserved
	if !m.HasSentInit("ATZ") {
		t.Error("ATZ should still be in initSent after reset")
	}
}

func TestReset_ATF_Clears_InitSent(t *testing.T) {
	m := New("test")

	// Send ATZ first
	m.Execute(ParseCommand("ATZ"))
	if !m.HasSentInit("ATZ") {
		t.Fatal("ATZ should be in initSent after executing it")
	}

	// Factory reset clears everything including initSent
	m.Execute(ParseCommand("AT&F"))

	// ATZ should be cleared
	if m.HasSentInit("ATZ") {
		t.Error("ATZ should be cleared from initSent after AT&F")
	}

	// AT&F itself should be recorded
	if !m.HasSentInit("AT&F") {
		t.Error("AT&F should be in initSent after factory reset")
	}

	// Verify settings are back to defaults
	if !m.Echo() {
		t.Error("expected echo=true after AT&F")
	}
	if !m.Verbose() {
		t.Error("expected verbose=true after AT&F")
	}
	if m.GetErrorCorrection() != 5 {
		t.Errorf("expected errorCorrection=5 after AT&F, got %d", m.GetErrorCorrection())
	}
	if !m.GetCompression() {
		t.Error("expected compression=true after AT&F")
	}
}

// ---------------------------------------------------------------------------
// initSent tracking
// ---------------------------------------------------------------------------

func TestInitSent_ATZ(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("ATZ"))
	if !m.HasSentInit("ATZ") {
		t.Error("HasSentInit(ATZ) should be true after executing ATZ")
	}
}

func TestInitSent_ATF(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("AT&F"))
	if !m.HasSentInit("AT&F") {
		t.Error("HasSentInit(AT&F) should be true after executing AT&F")
	}
}

func TestInitSent_ATZ_Cleared_By_ATF(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("ATZ"))
	if !m.HasSentInit("ATZ") {
		t.Fatal("ATZ should be tracked")
	}

	m.Execute(ParseCommand("AT&F"))
	if m.HasSentInit("ATZ") {
		t.Error("ATZ should be cleared after AT&F")
	}
}

func TestInitSent_CaseInsensitive(t *testing.T) {
	m := New("test")
	m.Execute(ParseCommand("ATZ"))

	if !m.HasSentInit("atz") {
		t.Error("HasSentInit should be case insensitive (atz)")
	}
	if !m.HasSentInit("Atz") {
		t.Error("HasSentInit should be case insensitive (Atz)")
	}
	if !m.HasSentInit("ATZ") {
		t.Error("HasSentInit should be case insensitive (ATZ)")
	}
}

// ---------------------------------------------------------------------------
// CmdHangup sets state from StateData to StateCommand
// ---------------------------------------------------------------------------

func TestHangup_FromDataState(t *testing.T) {
	m := New("test")
	m.SetState(StateData)
	if m.State() != StateData {
		t.Fatal("expected StateData")
	}
	m.Execute(ParseCommand("ATH"))
	if m.State() != StateCommand {
		t.Error("expected StateCommand after ATH from data state")
	}
}

// ---------------------------------------------------------------------------
// CmdLockSpeed — multiple speed codes
// ---------------------------------------------------------------------------

func TestLockSpeed_AllCodes(t *testing.T) {
	for code, expectedBaud := range SpeedCodeMap {
		t.Run(fmt.Sprintf("&N%d=%d", code, expectedBaud), func(t *testing.T) {
			m := New("test")
			cmd := ParseCommand(fmt.Sprintf("AT&N%d", code))
			m.Execute(cmd)
			if m.GetBaud() != expectedBaud {
				t.Errorf("AT&N%d: expected baud=%d, got %d", code, expectedBaud, m.GetBaud())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CmdSetX — all valid levels
// ---------------------------------------------------------------------------

func TestSetX_AllLevels(t *testing.T) {
	for level := 0; level <= 4; level++ {
		t.Run(fmt.Sprintf("X%d", level), func(t *testing.T) {
			m := New("test")
			cmd := ParseCommand(fmt.Sprintf("ATX%d", level))
			m.Execute(cmd)
			resp, _, _ := m.Execute(ParseCommand("AT&V"))
			expected := fmt.Sprintf("X%d", level)
			if !strings.Contains(resp, expected) {
				t.Errorf("expected %s in viewConfig, got: %s", expected, resp)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CmdSetErrorCorrection — toggle values
// ---------------------------------------------------------------------------

func TestSetErrorCorrection_Values(t *testing.T) {
	tests := []struct {
		cmd      string
		expected int
	}{
		{"AT&Q0", 0},
		{"AT&Q5", 5},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			m := New("test")
			m.Execute(ParseCommand(tt.cmd))
			if m.GetErrorCorrection() != tt.expected {
				t.Errorf("%s: expected errorCorrection=%d, got %d", tt.cmd, tt.expected, m.GetErrorCorrection())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// FormatConnectResult — X level boundary at 2400
// ---------------------------------------------------------------------------

func TestFormatConnectResult_SpeedBoundary(t *testing.T) {
	// At exactly 2400, protocol suffix should appear
	m := New("test")
	// defaults: xlevel=4, errorCorrection=5, compression=false
	m.Execute(ParseCommand("AT%C0"))
	got := m.FormatConnectResult(2400)
	want := "\r\nCONNECT 2400/ARQ/V42/LAPM\r\n"
	if got != want {
		t.Errorf("FormatConnectResult at 2400: got %q, want %q", got, want)
	}

	// At 2399, no protocol suffix
	got = m.FormatConnectResult(2399)
	want = "\r\nCONNECT 2399\r\n"
	if got != want {
		t.Errorf("FormatConnectResult at 2399: got %q, want %q", got, want)
	}
}
