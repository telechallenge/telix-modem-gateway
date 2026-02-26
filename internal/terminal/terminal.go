package terminal

// Terminal handles terminal I/O processing
type Terminal struct {
	width  int
	height int
}

// New creates a new terminal handler
func New() *Terminal {
	return &Terminal{
		width:  80,
		height: 24,
	}
}

// SetSize sets the terminal dimensions
func (t *Terminal) SetSize(width, height int) {
	t.width = width
	t.height = height
}

// Width returns the terminal width
func (t *Terminal) Width() int {
	return t.width
}

// Height returns the terminal height
func (t *Terminal) Height() int {
	return t.height
}

// StripANSI removes ANSI escape sequences from data
// Useful for logging or processing
func StripANSI(data []byte) []byte {
	result := make([]byte, 0, len(data))
	inEscape := false
	inCSI := false

	for i := 0; i < len(data); i++ {
		b := data[i]

		if inCSI {
			// CSI sequence ends with a letter
			if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') {
				inCSI = false
				inEscape = false
			}
			continue
		}

		if inEscape {
			if b == '[' {
				inCSI = true
			} else {
				// Two-character escape sequence
				inEscape = false
			}
			continue
		}

		if b == 0x1B { // ESC
			inEscape = true
			continue
		}

		result = append(result, b)
	}

	return result
}

// IsControlChar checks if a byte is a control character
func IsControlChar(b byte) bool {
	return b < 32 && b != '\t' && b != '\n' && b != '\r'
}

// FilterPrintable returns only printable characters and common control chars
func FilterPrintable(data []byte) []byte {
	result := make([]byte, 0, len(data))
	for _, b := range data {
		if b >= 32 && b < 127 {
			result = append(result, b)
		} else if b == '\t' || b == '\n' || b == '\r' {
			result = append(result, b)
		}
	}
	return result
}
