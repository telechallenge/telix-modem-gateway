package modem

import "sync"

// SRegisters holds the modem S-register values
type SRegisters struct {
	mu        sync.RWMutex
	registers map[int]int
}

// Default S-register values
var defaultSRegisters = map[int]int{
	0:  0,  // Auto-answer ring count (0 = disabled)
	1:  0,  // Ring counter
	2:  43, // Escape character (+)
	3:  13, // Carriage return character
	4:  10, // Line feed character
	5:  8,  // Backspace character
	6:  2,  // Wait for dial tone (seconds)
	7:  30, // Connection timeout (seconds)
	8:  2,  // Comma pause time (seconds)
	9:  6,  // Carrier detect response time (1/10 sec)
	10: 7,  // Carrier loss delay (1/10 sec)
	12: 50, // Escape guard time (1/50 sec)
}

// NewSRegisters creates a new S-register set with default values
func NewSRegisters() *SRegisters {
	s := &SRegisters{
		registers: make(map[int]int),
	}
	s.Reset()
	return s
}

// Reset restores all registers to default values
func (s *SRegisters) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range defaultSRegisters {
		s.registers[k] = v
	}
}

// Get returns the value of a register
func (s *SRegisters) Get(reg int) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if val, ok := s.registers[reg]; ok {
		return val
	}
	return 0
}

// registerLimit defines the valid range for a register.
type registerLimit struct {
	min, max int
}

// registerLimits defines per-register valid ranges. Values are silently
// clamped to these ranges, matching Hayes modem behavior.
var registerLimits = map[int]registerLimit{
	0:  {0, 255},  // Auto-answer ring count
	2:  {0, 127},  // Escape char (valid ASCII; 0 disables escape — Hayes feature)
	3:  {1, 31},   // CR must be a control char
	4:  {1, 31},   // LF must be a control char
	5:  {1, 31},   // BS must be a control char
	6:  {0, 10},   // Dial tone wait, cap at 10 seconds
	7:  {1, 60},   // Connection timeout, at least 1s, cap at 60s
	8:  {0, 10},   // Comma pause time
	9:  {1, 255},  // Carrier detect time
	10: {1, 255},  // Carrier loss delay
	12: {20, 255}, // Guard time, minimum ~400ms to prevent trivial escape
}

// Set sets the value of a register, clamping to the valid range.
// Per-register semantic limits are enforced where defined; all other
// registers are clamped to 0-255.
func (s *SRegisters) Set(reg, value int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if limit, ok := registerLimits[reg]; ok {
		if value < limit.min {
			value = limit.min
		} else if value > limit.max {
			value = limit.max
		}
	} else {
		if value < 0 {
			value = 0
		} else if value > 255 {
			value = 255
		}
	}
	s.registers[reg] = value
}

// GetEscapeChar returns the escape character (S2)
func (s *SRegisters) GetEscapeChar() byte {
	return byte(s.Get(2))
}

// GetCR returns the carriage return character (S3)
func (s *SRegisters) GetCR() byte {
	return byte(s.Get(3))
}

// GetLF returns the line feed character (S4)
func (s *SRegisters) GetLF() byte {
	return byte(s.Get(4))
}

// GetBackspace returns the backspace character (S5)
func (s *SRegisters) GetBackspace() byte {
	return byte(s.Get(5))
}

// GetDialToneWait returns the dial tone wait time in seconds (S6)
func (s *SRegisters) GetDialToneWait() int {
	return s.Get(6)
}

// GetConnectionTimeout returns the connection timeout in seconds (S7)
func (s *SRegisters) GetConnectionTimeout() int {
	return s.Get(7)
}

// GetCarrierDetectTime returns the carrier detect response time in milliseconds (S9)
func (s *SRegisters) GetCarrierDetectTime() int {
	return s.Get(9) * 100
}

// GetEscapeGuardTime returns the escape guard time in milliseconds
func (s *SRegisters) GetEscapeGuardTime() int {
	// S12 is in 1/50 second units, convert to milliseconds
	return s.Get(12) * 20
}

// All returns a copy of all registers
func (s *SRegisters) All() map[int]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[int]int)
	for k, v := range s.registers {
		result[k] = v
	}
	return result
}
