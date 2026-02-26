package modem

import (
	"regexp"
	"strconv"
	"strings"
)

// CommandType represents the type of AT command
type CommandType int

const (
	CmdUnknown CommandType = iota
	CmdAT              // Just AT
	CmdReset           // ATZ
	CmdFactoryReset    // AT&F
	CmdDial            // ATDT or ATDP
	CmdHangup          // ATH or ATH0
	CmdOffHook         // ATH1
	CmdEchoOff         // ATE0
	CmdEchoOn          // ATE1
	CmdVerboseOff      // ATV0
	CmdVerboseOn       // ATV1
	CmdQuietOff        // ATQ0
	CmdQuietOn         // ATQ1
	CmdIdentify        // ATI
	CmdViewConfig      // AT&V
	CmdSRegisterSet    // ATS<n>=<v>
	CmdSRegisterQuery  // ATS<n>?
	CmdSetX            // ATX0-ATX4
	CmdLockSpeed           // AT&N<n>
	CmdSetErrorCorrection  // AT&Q0, AT&Q5
	CmdSetCompression      // AT%C0, AT%C1
)

// Command represents a parsed AT command
type Command struct {
	Type     CommandType
	Raw      string
	Number   string // For dial commands
	Register int    // For S-register commands
	Value    int    // For S-register set commands
}

// SpeedCodeMap maps AT&N code numbers to baud rates. Code 0 means auto.
var SpeedCodeMap = map[int]int{
	0: 0, 1: 300, 2: 1200, 3: 2400, 6: 4800, 7: 7200,
	8: 9600, 10: 14400, 11: 19200, 12: 38400, 14: 56000,
}

var (
	sRegSetPattern   = regexp.MustCompile(`(?i)^ATS(\d+)=(\d+)$`)
	sRegQueryPattern = regexp.MustCompile(`(?i)^ATS(\d+)\?$`)
	dialPattern      = regexp.MustCompile(`(?i)^ATD[TP]([0-9\-\(\)\.\*# ]+)$`)
	lockSpeedPattern      = regexp.MustCompile(`(?i)^AT&N(\d+)$`)
	errorCorrPattern      = regexp.MustCompile(`(?i)^AT&Q(\d)$`)
	compressionPattern    = regexp.MustCompile(`(?i)^AT%C(\d)$`)
)

// ParseCommand parses an AT command string
func ParseCommand(input string) *Command {
	input = strings.TrimSpace(input)
	upper := strings.ToUpper(input)

	cmd := &Command{
		Type: CmdUnknown,
		Raw:  input,
	}

	// Must start with AT
	if !strings.HasPrefix(upper, "AT") {
		return cmd
	}

	// Just AT
	if upper == "AT" {
		cmd.Type = CmdAT
		return cmd
	}

	// Check specific commands
	switch upper {
	case "ATZ":
		cmd.Type = CmdReset
		return cmd
	case "AT&F":
		cmd.Type = CmdFactoryReset
		return cmd
	case "ATH", "ATH0":
		cmd.Type = CmdHangup
		return cmd
	case "ATH1":
		cmd.Type = CmdOffHook
		return cmd
	case "ATE0":
		cmd.Type = CmdEchoOff
		return cmd
	case "ATE1":
		cmd.Type = CmdEchoOn
		return cmd
	case "ATV0":
		cmd.Type = CmdVerboseOff
		return cmd
	case "ATV1":
		cmd.Type = CmdVerboseOn
		return cmd
	case "ATQ0":
		cmd.Type = CmdQuietOff
		return cmd
	case "ATQ1":
		cmd.Type = CmdQuietOn
		return cmd
	case "ATI":
		cmd.Type = CmdIdentify
		return cmd
	case "AT&V":
		cmd.Type = CmdViewConfig
		return cmd
	case "ATX0", "ATX1", "ATX2", "ATX3", "ATX4":
		cmd.Type = CmdSetX
		cmd.Value = int(upper[3] - '0')
		return cmd
	}

	// Check dial command
	if matches := dialPattern.FindStringSubmatch(input); matches != nil {
		cmd.Type = CmdDial
		cmd.Number = matches[1]
		return cmd
	}

	// Check S-register set
	if matches := sRegSetPattern.FindStringSubmatch(input); matches != nil {
		reg, err1 := strconv.Atoi(matches[1])
		val, err2 := strconv.Atoi(matches[2])
		if err1 != nil || err2 != nil || reg < 0 || reg > 255 {
			return cmd // CmdUnknown
		}
		cmd.Type = CmdSRegisterSet
		cmd.Register = reg
		cmd.Value = val
		return cmd
	}

	// Check S-register query
	if matches := sRegQueryPattern.FindStringSubmatch(input); matches != nil {
		reg, err := strconv.Atoi(matches[1])
		if err != nil || reg < 0 || reg > 255 {
			return cmd // CmdUnknown
		}
		cmd.Type = CmdSRegisterQuery
		cmd.Register = reg
		return cmd
	}

	// Check AT&N lock speed
	if matches := lockSpeedPattern.FindStringSubmatch(input); matches != nil {
		code, err := strconv.Atoi(matches[1])
		if err != nil {
			return cmd // CmdUnknown
		}
		if _, ok := SpeedCodeMap[code]; !ok {
			return cmd // CmdUnknown — invalid speed code
		}
		cmd.Type = CmdLockSpeed
		cmd.Value = code
		return cmd
	}

	// Check AT&Q error correction
	if matches := errorCorrPattern.FindStringSubmatch(input); matches != nil {
		val, err := strconv.Atoi(matches[1])
		if err != nil {
			return cmd // CmdUnknown
		}
		if val != 0 && val != 5 {
			return cmd // CmdUnknown — only 0 and 5 are valid
		}
		cmd.Type = CmdSetErrorCorrection
		cmd.Value = val
		return cmd
	}

	// Check AT%C compression
	if matches := compressionPattern.FindStringSubmatch(input); matches != nil {
		val, err := strconv.Atoi(matches[1])
		if err != nil {
			return cmd // CmdUnknown
		}
		if val != 0 && val != 1 {
			return cmd // CmdUnknown — only 0 and 1 are valid
		}
		cmd.Type = CmdSetCompression
		cmd.Value = val
		return cmd
	}

	return cmd
}

// ResultCode represents modem result codes
type ResultCode int

const (
	ResultOK        ResultCode = 0
	ResultConnect   ResultCode = 1
	ResultRing      ResultCode = 2
	ResultNoCarrier ResultCode = 3
	ResultError     ResultCode = 4
	ResultNoAnswer  ResultCode = 8
)

func (r ResultCode) String() string {
	switch r {
	case ResultOK:
		return "OK"
	case ResultConnect:
		return "CONNECT"
	case ResultRing:
		return "RING"
	case ResultNoCarrier:
		return "NO CARRIER"
	case ResultError:
		return "ERROR"
	case ResultNoAnswer:
		return "NO ANSWER"
	default:
		return "ERROR"
	}
}
