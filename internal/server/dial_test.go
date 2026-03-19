package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"telix/internal/config"
	"telix/internal/logging"
	"telix/internal/modem"
)

func TestPickConnectSpeed(t *testing.T) {
	validSpeeds := map[int]bool{
		56000: true, 33600: true, 31200: true, 28800: true, 24000: true,
	}

	// Run enough times to verify we only get valid speeds
	for i := 0; i < 100; i++ {
		speed := pickConnectSpeed()
		if !validSpeeds[speed] {
			t.Errorf("pickConnectSpeed() returned unexpected speed: %d", speed)
		}
	}
}

func TestConnectSpeedsNotEmpty(t *testing.T) {
	if len(connectSpeeds) == 0 {
		t.Fatal("connectSpeeds pool is empty")
	}
}

func newTestSession(t *testing.T) *Session {
	t.Helper()
	logger, err := logging.New("error", "text", "")
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		modem:  modem.New("test"),
		logger: logger.WithSession("test-session", "127.0.0.1"),
	}
}

func TestCheckRequiredSettings_NoRequirements(t *testing.T) {
	s := newTestSession(t)
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
	}
	if !s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected true when no requirements set")
	}
}

func TestCheckRequiredSettings_InitRequired_NotSent(t *testing.T) {
	s := newTestSession(t)
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Init: "ATZ",
		},
	}
	if s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected false when required init not sent")
	}
}

func TestCheckRequiredSettings_InitRequired_Sent(t *testing.T) {
	s := newTestSession(t)
	s.modem.Execute(modem.ParseCommand("ATZ"))
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Init: "ATZ",
		},
	}
	if !s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected true when required init was sent")
	}
}

func TestCheckRequiredSettings_BaudRequired_Wrong(t *testing.T) {
	s := newTestSession(t)
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Baud: 9600,
		},
	}
	if s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected false when baud doesn't match")
	}
}

func TestCheckRequiredSettings_BaudRequired_Correct(t *testing.T) {
	s := newTestSession(t)
	s.modem.Execute(modem.ParseCommand("AT&N8"))
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Baud: 9600,
		},
	}
	if !s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected true when baud matches")
	}
}

func TestCheckRequiredSettings_ErrorCorrectionRequired_Wrong(t *testing.T) {
	s := newTestSession(t)
	ecOff := false
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			ErrorCorrection: &ecOff,
		},
	}
	if s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected false when error correction doesn't match")
	}
}

func TestCheckRequiredSettings_CompressionRequired_Wrong(t *testing.T) {
	s := newTestSession(t)
	compOff := false
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Compression: &compOff,
		},
	}
	if s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected false when compression doesn't match")
	}
}

func TestCheckRequiredSettings_AllRequired_AllCorrect(t *testing.T) {
	s := newTestSession(t)
	s.modem.Execute(modem.ParseCommand("ATZ"))
	s.modem.Execute(modem.ParseCommand("AT&N8"))
	ecOn := true
	compOn := true
	entry := &config.PhonebookEntry{
		Number: "555-1212",
		Host:   "localhost",
		Port:   23,
		RequiredSettings: config.RequiredSettings{
			Init:            "ATZ",
			Baud:            9600,
			ErrorCorrection: &ecOn,
			Compression:     &compOn,
		},
	}
	if !s.checkRequiredSettings("555-1212", entry) {
		t.Error("expected true when all settings match")
	}
}

func TestIsConnectionRefused(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name: "connection refused",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &os.SyscallError{
					Syscall: "connect",
					Err:     syscall.ECONNREFUSED,
				},
			},
			expected: true,
		},
		{
			name: "connection timeout",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &os.SyscallError{
					Syscall: "connect",
					Err:     syscall.ETIMEDOUT,
				},
			},
			expected: false,
		},
		{
			name:     "generic error",
			err:      fmt.Errorf("some error"),
			expected: false,
		},
		{
			name:     "nil-safe wrapped error",
			err:      errors.New("connection failed"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isConnectionRefused(tt.err)
			if result != tt.expected {
				t.Errorf("isConnectionRefused(%v) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}
