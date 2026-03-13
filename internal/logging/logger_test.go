package logging

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- sanitizeField ---

func TestSanitizeField_TruncatesToMaxLen(t *testing.T) {
	input := "abcdefghij"
	got := sanitizeField(input, 5)
	if got != "abcde" {
		t.Errorf("expected %q, got %q", "abcde", got)
	}
}

func TestSanitizeField_StripsNonPrintable(t *testing.T) {
	input := "hello\x00world\x01\x1f\x7f"
	got := sanitizeField(input, 100)
	if got != "helloworld" {
		t.Errorf("expected %q, got %q", "helloworld", got)
	}
}

func TestSanitizeField_KeepsPrintableASCII(t *testing.T) {
	input := " !@#abc~}" // all in 32-126 range
	got := sanitizeField(input, 100)
	if got != input {
		t.Errorf("expected %q, got %q", input, got)
	}
}

func TestSanitizeField_EmptyString(t *testing.T) {
	got := sanitizeField("", 100)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestSanitizeField_ShorterThanMaxLen(t *testing.T) {
	input := "short"
	got := sanitizeField(input, 100)
	if got != "short" {
		t.Errorf("expected %q, got %q", "short", got)
	}
}

// --- Level ---

func TestParseLevel_ValidLevels(t *testing.T) {
	tests := []struct {
		input    string
		expected Level
	}{
		{"debug", DebugLevel},
		{"info", InfoLevel},
		{"warn", WarnLevel},
		{"error", ErrorLevel},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := ParseLevel(tc.input)
			if got != tc.expected {
				t.Errorf("ParseLevel(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestParseLevel_InvalidDefaults(t *testing.T) {
	tests := []string{"", "trace", "WARN", "Info", "garbage"}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			got := ParseLevel(input)
			if got != InfoLevel {
				t.Errorf("ParseLevel(%q) = %v, want InfoLevel", input, got)
			}
		})
	}
}

func TestLevelString_RoundTrips(t *testing.T) {
	levels := []Level{DebugLevel, InfoLevel, WarnLevel, ErrorLevel}
	for _, lvl := range levels {
		t.Run(lvl.String(), func(t *testing.T) {
			s := lvl.String()
			got := ParseLevel(s)
			if got != lvl {
				t.Errorf("ParseLevel(%q) = %v, want %v", s, got, lvl)
			}
		})
	}
}

// --- Logger creation ---

func TestNew_NoFile_WritesToStdout(t *testing.T) {
	logger, err := New("info", "json", "")
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	if logger == nil {
		t.Fatal("New() returned nil logger")
	}
}

func TestNew_WithTempFile_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "test.log")

	logger, err := New("info", "json", logFile)
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	if logger == nil {
		t.Fatal("New() returned nil logger")
	}

	// Write a message to force the file to be created by lumberjack
	logger.Info().Str("key", "value").Msg("init")

	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Fatalf("log file %q was not created", logFile)
	}
}

// --- Event chaining ---

func TestEventChaining_Str(t *testing.T) {
	logger, _ := New("debug", "text", "")
	e := logger.Info()
	result := e.Str("key", "val")
	if result != e {
		t.Error("Str() did not return the same event for chaining")
	}
}

func TestEventChaining_Int(t *testing.T) {
	logger, _ := New("debug", "text", "")
	e := logger.Info()
	result := e.Int("count", 42)
	if result != e {
		t.Error("Int() did not return the same event for chaining")
	}
}

func TestEventChaining_Err(t *testing.T) {
	logger, _ := New("debug", "text", "")
	e := logger.Info()
	result := e.Err(errors.New("something"))
	if result != e {
		t.Error("Err() did not return the same event for chaining")
	}
}

func TestEventChaining_ErrNil(t *testing.T) {
	logger, _ := New("debug", "json", "")
	e := logger.Info()
	e.Err(nil)
	if _, ok := e.fields["error"]; ok {
		t.Error("Err(nil) should not add an 'error' field")
	}
}

// --- JSON output ---

func TestJSONOutput_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "json_test.log")

	logger, err := New("debug", "json", logFile)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	logger.Info().Str("user", "alice").Int("age", 30).Msg("hello world")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		t.Fatal("log file is empty")
	}

	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("invalid JSON in log: %v\nraw: %s", err, lines[len(lines)-1])
	}

	if entry["level"] != "info" {
		t.Errorf("level = %v, want 'info'", entry["level"])
	}
	if entry["message"] != "hello world" {
		t.Errorf("message = %v, want 'hello world'", entry["message"])
	}
	if entry["user"] != "alice" {
		t.Errorf("user = %v, want 'alice'", entry["user"])
	}
	if _, ok := entry["timestamp"]; !ok {
		t.Error("missing 'timestamp' field in JSON output")
	}
	// age is float64 from JSON unmarshal
	if age, ok := entry["age"].(float64); !ok || age != 30 {
		t.Errorf("age = %v, want 30", entry["age"])
	}
}

// --- SessionLogger ---

func TestWithSession_CreatesSessionLogger(t *testing.T) {
	logger, _ := New("debug", "json", "")
	sl := logger.WithSession("sess-123", "192.168.1.1")
	if sl == nil {
		t.Fatal("WithSession() returned nil")
	}
	if sl.sessionID != "sess-123" {
		t.Errorf("sessionID = %q, want %q", sl.sessionID, "sess-123")
	}
	if sl.sourceIP != "192.168.1.1" {
		t.Errorf("sourceIP = %q, want %q", sl.sourceIP, "192.168.1.1")
	}
}

func TestSessionLogger_MethodsContainSessionFields(t *testing.T) {
	// Each method gets its own log file to avoid truncation issues with lumberjack
	methods := []struct {
		name string
		call func(sl *SessionLogger)
	}{
		{"NewConnection", func(sl *SessionLogger) { sl.NewConnection() }},
		{"Disconnected", func(sl *SessionLogger) { sl.Disconnected() }},
		{"IdleTimeout", func(sl *SessionLogger) { sl.IdleTimeout() }},
		{"InvalidCommand", func(sl *SessionLogger) { sl.InvalidCommand("BAD") }},
		{"InvalidNumber", func(sl *SessionLogger) { sl.InvalidNumber("999") }},
		{"ConnectionAttempt", func(sl *SessionLogger) { sl.ConnectionAttempt("5551234", "success") }},
		{"MissingSettings", func(sl *SessionLogger) { sl.MissingSettings("5551234", "baud_rate", "not configured") }},
	}

	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			dir := t.TempDir()
			logFile := filepath.Join(dir, m.name+".log")

			logger, err := New("debug", "json", logFile)
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}

			sl := logger.WithSession("sess-abc", "10.0.0.1")
			m.call(sl)

			data, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatalf("reading log: %v", err)
			}

			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			lastLine := lines[len(lines)-1]

			var entry map[string]interface{}
			if err := json.Unmarshal([]byte(lastLine), &entry); err != nil {
				t.Fatalf("invalid JSON: %v\nraw: %s", err, lastLine)
			}

			if entry["session_id"] != "sess-abc" {
				t.Errorf("session_id = %v, want 'sess-abc'", entry["session_id"])
			}
			if entry["source_ip"] != "10.0.0.1" {
				t.Errorf("source_ip = %v, want '10.0.0.1'", entry["source_ip"])
			}
		})
	}
}

func TestRateLimited_NoSessionContext(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "ratelimit_test.log")

	logger, err := New("debug", "json", logFile)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	logger.RateLimited("172.16.0.1", "too_many_connections")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}

	var entry map[string]interface{}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if entry["event"] != "rate_limited" {
		t.Errorf("event = %v, want 'rate_limited'", entry["event"])
	}
	if entry["source_ip"] != "172.16.0.1" {
		t.Errorf("source_ip = %v, want '172.16.0.1'", entry["source_ip"])
	}
	if entry["reason"] != "too_many_connections" {
		t.Errorf("reason = %v, want 'too_many_connections'", entry["reason"])
	}
	if _, ok := entry["session_id"]; ok {
		t.Error("RateLimited should not include session_id")
	}
}

// --- Level filtering ---

func TestLevelFiltering_DebugNotLoggedAtInfoLevel(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "filter_test.log")

	logger, err := New("info", "json", logFile)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	logger.Debug().Str("key", "val").Msg("should not appear")

	data, err := os.ReadFile(logFile)
	if err != nil {
		// File may not exist if nothing was written — that's fine
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("reading log: %v", err)
	}

	content := strings.TrimSpace(string(data))
	if content != "" {
		t.Errorf("debug message should not be logged at info level, got: %s", content)
	}
}

func TestLevelFiltering_InfoLoggedAtInfoLevel(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "filter_test.log")

	logger, err := New("info", "json", logFile)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	logger.Info().Str("key", "val").Msg("should appear")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}

	if !strings.Contains(string(data), "should appear") {
		t.Error("info message should be logged at info level")
	}
}

func TestLevelFiltering_WarnLoggedAtInfoLevel(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "filter_test.log")

	logger, err := New("info", "json", logFile)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	logger.Warn().Str("key", "val").Msg("warning message")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}

	if !strings.Contains(string(data), "warning message") {
		t.Error("warn message should be logged at info level")
	}
}
