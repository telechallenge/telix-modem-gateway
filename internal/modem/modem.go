package modem

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// State represents the modem's current state
type State int

const (
	StateCommand State = iota
	StateData
	StateEscaping
)

// Modem represents a virtual modem instance
type Modem struct {
	mu sync.Mutex

	state           State
	echo            bool
	verbose         bool
	quiet           bool
	xlevel          int  // X command level (0-4), controls result code detail
	baud            int  // Locked baud rate (0 = auto/56000)
	errorCorrection int  // 0 = off, 5 = V.42/LAPM
	compression     bool // V.42bis
	registers       *SRegisters
	initSent        map[string]bool // Track which init commands have been sent
}

// New creates a new modem instance
func New() *Modem {
	m := &Modem{
		state:           StateCommand,
		echo:            true,
		verbose:         true,
		quiet:           false,
		xlevel:          4,
		errorCorrection: 5,
		compression:     true,
		registers:       NewSRegisters(),
		initSent:        make(map[string]bool),
	}
	return m
}

// State returns the current modem state
func (m *Modem) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// SetState sets the modem state
func (m *Modem) SetState(s State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = s
}

// Echo returns whether echo is enabled
func (m *Modem) Echo() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.echo
}

// Verbose returns whether verbose mode is enabled
func (m *Modem) Verbose() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.verbose
}

// Quiet returns whether quiet mode is enabled
func (m *Modem) Quiet() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.quiet
}

// Registers returns the S-registers
func (m *Modem) Registers() *SRegisters {
	return m.registers
}

// GetBaud returns the locked baud rate. 0 means auto (effectively 56000).
func (m *Modem) GetBaud() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baud
}

// GetErrorCorrection returns the error correction mode. 0 = off, 5 = V.42/LAPM.
func (m *Modem) GetErrorCorrection() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.errorCorrection
}

// GetCompression returns whether V.42bis compression is enabled.
func (m *Modem) GetCompression() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.compression
}

// HasSentInit checks if an init command was sent
func (m *Modem) HasSentInit(cmd string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initSent[strings.ToUpper(cmd)]
}

// Execute executes an AT command and returns the response
func (m *Modem) Execute(cmd *Command) (string, ResultCode, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cr := m.registers.GetCR()
	lf := m.registers.GetLF()

	// Track init commands
	if cmd.Type == CmdReset || cmd.Type == CmdFactoryReset {
		m.initSent[strings.ToUpper(cmd.Raw)] = true
	}

	switch cmd.Type {
	case CmdAT:
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdReset:
		m.registers.Reset()
		m.echo = true
		m.verbose = true
		m.quiet = false
		m.xlevel = 4
		m.baud = 0
		m.errorCorrection = 5
		m.compression = true
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdFactoryReset:
		m.registers.Reset()
		m.echo = true
		m.verbose = true
		m.quiet = false
		m.xlevel = 4
		m.baud = 0
		m.errorCorrection = 5
		m.compression = true
		m.initSent = make(map[string]bool)
		m.initSent["AT&F"] = true
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdHangup:
		m.state = StateCommand
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdOffHook:
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdEchoOff:
		m.echo = false
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdEchoOn:
		m.echo = true
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdVerboseOff:
		m.verbose = false
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdVerboseOn:
		m.verbose = true
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdQuietOff:
		m.quiet = false
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdQuietOn:
		m.quiet = true
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdIdentify:
		info := fmt.Sprintf("%c%cTelix Modem Gateway v1.0%c%c", cr, lf, cr, lf)
		info += fmt.Sprintf("Hayes Compatible 56K%c%c", cr, lf)
		return info + m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdViewConfig:
		return m.viewConfig(cr, lf), ResultOK, false

	case CmdSRegisterQuery:
		val := m.registers.Get(cmd.Register)
		return fmt.Sprintf("%c%c%03d%c%c", cr, lf, val, cr, lf) + m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdSRegisterSet:
		m.registers.Set(cmd.Register, cmd.Value)
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdSetX:
		if cmd.Value >= 0 && cmd.Value <= 4 {
			m.xlevel = cmd.Value
		}
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdLockSpeed:
		m.baud = SpeedCodeMap[cmd.Value]
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdSetErrorCorrection:
		m.errorCorrection = cmd.Value
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdSetCompression:
		m.compression = cmd.Value == 1
		return m.formatResult(ResultOK, cr, lf), ResultOK, false

	case CmdDial:
		// Return a flag indicating we should dial
		return "", ResultOK, true

	default:
		return m.formatResult(ResultError, cr, lf), ResultError, false
	}
}

func (m *Modem) formatResult(code ResultCode, cr, lf byte) string {
	if m.quiet {
		return ""
	}
	if m.verbose {
		return fmt.Sprintf("%c%c%s%c%c", cr, lf, code.String(), cr, lf)
	}
	return fmt.Sprintf("%d%c", code, cr)
}

// FormatResultExternal formats a result code using current settings
func (m *Modem) FormatResultExternal(code ResultCode) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.formatResult(code, m.registers.GetCR(), m.registers.GetLF())
}

// FormatConnectResult formats a CONNECT result with speed based on X level.
// X0 returns bare "CONNECT", X1+ returns "CONNECT <speed>".
func (m *Modem) FormatConnectResult(speed int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cr := m.registers.GetCR()
	lf := m.registers.GetLF()
	if m.quiet {
		return ""
	}
	if m.verbose {
		if m.xlevel >= 1 {
			suffix := ""
			if speed >= 2400 && m.errorCorrection == 5 {
				suffix = "/ARQ/V42/LAPM"
				if m.compression {
					suffix += "/V42BIS"
				}
			}
			return fmt.Sprintf("%c%cCONNECT %d%s%c%c", cr, lf, speed, suffix, cr, lf)
		}
		return fmt.Sprintf("%c%cCONNECT%c%c", cr, lf, cr, lf)
	}
	return fmt.Sprintf("%d%c", ResultConnect, cr)
}

func (m *Modem) viewConfig(cr, lf byte) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("%c%cACTIVE PROFILE:%c%c", cr, lf, cr, lf))
	baudStr := "auto"
	if m.baud > 0 {
		baudStr = fmt.Sprintf("%d", m.baud)
	}
	sb.WriteString(fmt.Sprintf("E%d V%d Q%d X%d &N=%s &Q%d %%C%d%c%c",
		boolToInt(m.echo),
		boolToInt(m.verbose),
		boolToInt(m.quiet),
		m.xlevel,
		baudStr,
		m.errorCorrection,
		boolToInt(m.compression),
		cr, lf))

	sb.WriteString(fmt.Sprintf("%c%cS-REGISTERS:%c%c", cr, lf, cr, lf))

	regs := m.registers.All()
	keys := make([]int, 0, len(regs))
	for k := range regs {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	for _, k := range keys {
		sb.WriteString(fmt.Sprintf("S%02d=%03d ", k, regs[k]))
	}
	sb.WriteString(fmt.Sprintf("%c%c", cr, lf))

	sb.WriteString(m.formatResult(ResultOK, cr, lf))
	return sb.String()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
