package modem

import "testing"

func TestSRegistersDefaults(t *testing.T) {
	s := NewSRegisters()

	tests := []struct {
		reg      int
		expected int
	}{
		{0, 0},   // Auto-answer disabled
		{1, 0},   // Ring counter
		{2, 43},  // Escape char (+)
		{3, 13},  // CR
		{4, 10},  // LF
		{5, 8},   // Backspace
		{6, 2},   // Dial tone wait
		{7, 30},  // Connection timeout
		{8, 2},   // Comma pause
		{12, 50}, // Escape guard time
	}

	for _, test := range tests {
		val := s.Get(test.reg)
		if val != test.expected {
			t.Errorf("S%d = %d, want %d", test.reg, val, test.expected)
		}
	}
}

func TestSRegistersSetGet(t *testing.T) {
	s := NewSRegisters()

	s.Set(7, 30)
	if s.Get(7) != 30 {
		t.Errorf("S7 = %d, want 30", s.Get(7))
	}

	s.Set(0, 5)
	if s.Get(0) != 5 {
		t.Errorf("S0 = %d, want 5", s.Get(0))
	}
}

func TestSRegistersReset(t *testing.T) {
	s := NewSRegisters()

	s.Set(7, 99)
	s.Set(0, 5)
	s.Reset()

	if s.Get(7) != 30 {
		t.Errorf("After reset, S7 = %d, want 30", s.Get(7))
	}
	if s.Get(0) != 0 {
		t.Errorf("After reset, S0 = %d, want 0", s.Get(0))
	}
}

func TestSRegistersHelperMethods(t *testing.T) {
	s := NewSRegisters()

	if s.GetEscapeChar() != '+' {
		t.Errorf("GetEscapeChar() = %c, want +", s.GetEscapeChar())
	}

	if s.GetCR() != '\r' {
		t.Errorf("GetCR() = %d, want 13", s.GetCR())
	}

	if s.GetLF() != '\n' {
		t.Errorf("GetLF() = %d, want 10", s.GetLF())
	}

	if s.GetBackspace() != 8 {
		t.Errorf("GetBackspace() = %d, want 8", s.GetBackspace())
	}

	if s.GetConnectionTimeout() != 30 {
		t.Errorf("GetConnectionTimeout() = %d, want 30", s.GetConnectionTimeout())
	}

	// S12 is in 1/50 second units, so 50 * 20 = 1000ms
	if s.GetEscapeGuardTime() != 1000 {
		t.Errorf("GetEscapeGuardTime() = %d, want 1000", s.GetEscapeGuardTime())
	}
}
