package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

type Level int

const (
	DebugLevel Level = iota
	InfoLevel
	WarnLevel
	ErrorLevel
)

func (l Level) String() string {
	switch l {
	case DebugLevel:
		return "debug"
	case InfoLevel:
		return "info"
	case WarnLevel:
		return "warn"
	case ErrorLevel:
		return "error"
	default:
		return "info"
	}
}

func ParseLevel(s string) Level {
	switch s {
	case "debug":
		return DebugLevel
	case "info":
		return InfoLevel
	case "warn":
		return WarnLevel
	case "error":
		return ErrorLevel
	default:
		return InfoLevel
	}
}

type Logger struct {
	mu     sync.Mutex
	output io.Writer
	// level is atomic because SetLevel can move it while sessions are logging,
	// and the filter in Msg is deliberately outside the output mutex — an
	// event below the threshold should cost nothing and contend with nothing.
	level atomic.Int32
	json  bool
}

// SetLevel changes the threshold on a running logger, so a live gateway can be
// turned up to debug and back down without dropping its callers.
func (l *Logger) SetLevel(level string) {
	l.level.Store(int32(ParseLevel(level)))
}

// Level reports the current threshold.
func (l *Logger) Level() Level {
	return Level(l.level.Load())
}

func New(level, format, file string) (*Logger, error) {
	var output io.Writer = os.Stdout

	if file != "" {
		dir := filepath.Dir(file)
		if err := os.MkdirAll(dir, 0750); err != nil {
			return nil, err
		}

		logFile := &lumberjack.Logger{
			Filename:   file,
			MaxSize:    50, // megabytes
			MaxBackups: 3,
			MaxAge:     28, // days
		}
		output = io.MultiWriter(os.Stdout, logFile)
	}

	l := &Logger{
		output: output,
		json:   format == "json",
	}
	l.level.Store(int32(ParseLevel(level)))
	return l, nil
}

type Event struct {
	logger *Logger
	level  Level
	fields map[string]interface{}
}

func (l *Logger) newEvent(level Level) *Event {
	return &Event{
		logger: l,
		level:  level,
		fields: make(map[string]interface{}),
	}
}

func (l *Logger) Info() *Event {
	return l.newEvent(InfoLevel)
}

func (l *Logger) Warn() *Event {
	return l.newEvent(WarnLevel)
}

func (l *Logger) Error() *Event {
	return l.newEvent(ErrorLevel)
}

func (l *Logger) Debug() *Event {
	return l.newEvent(DebugLevel)
}

func (e *Event) Str(key, val string) *Event {
	e.fields[key] = val
	return e
}

func (e *Event) Int(key string, val int) *Event {
	e.fields[key] = val
	return e
}

func (e *Event) Err(err error) *Event {
	if err != nil {
		e.fields["error"] = err.Error()
	}
	return e
}

func (e *Event) Msg(msg string) {
	if e.level < e.logger.Level() {
		return
	}

	e.logger.mu.Lock()
	defer e.logger.mu.Unlock()

	e.fields["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	e.fields["level"] = e.level.String()
	if msg != "" {
		e.fields["message"] = msg
	}

	if e.logger.json {
		data, err := json.Marshal(e.fields)
		if err != nil {
			fmt.Fprintf(e.logger.output,
				"{\"error\":\"json_marshal_failed\",\"timestamp\":\"%s\",\"level\":\"%s\"}\n",
				e.fields["timestamp"], e.level.String())
			return
		}
		fmt.Fprintln(e.logger.output, string(data))
	} else {
		fmt.Fprintf(e.logger.output, "%s [%s] %v\n",
			e.fields["timestamp"],
			e.level.String(),
			e.fields)
	}
}

// sanitizeField truncates and strips non-printable characters from a
// string to prevent log injection and abuse.
func sanitizeField(s string, maxLen int) string {
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	sanitized := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= 32 && s[i] < 127 {
			sanitized = append(sanitized, s[i])
		}
	}
	return string(sanitized)
}

// SessionLogger wraps Logger with a session ID and source IP baked in.
type SessionLogger struct {
	*Logger
	sessionID string
	sourceIP  string
}

// WithSession creates a SessionLogger for a specific session.
func (l *Logger) WithSession(sessionID, sourceIP string) *SessionLogger {
	return &SessionLogger{Logger: l, sessionID: sessionID, sourceIP: sourceIP}
}

func (sl *SessionLogger) event(level Level, event string) *Event {
	e := sl.Logger.newEvent(level)
	e.fields["event"] = event
	e.fields["session_id"] = sl.sessionID
	e.fields["source_ip"] = sl.sourceIP
	return e
}

func (sl *SessionLogger) IdleTimeout() {
	sl.event(InfoLevel, "idle_timeout").Msg("")
}

func (sl *SessionLogger) ConnectionAttempt(dialedNumber, result string) {
	sl.event(InfoLevel, "connection_attempt").
		Str("dialed_number", sanitizeField(dialedNumber, 100)).
		Str("result", result).
		Msg("")
}

func (sl *SessionLogger) InvalidCommand(command string) {
	sl.event(WarnLevel, "invalid_command").
		Str("command", sanitizeField(command, 100)).
		Msg("")
}

func (sl *SessionLogger) InvalidNumber(number string) {
	sl.event(WarnLevel, "invalid_number").
		Str("dialed_number", sanitizeField(number, 100)).
		Msg("")
}

func (sl *SessionLogger) NewConnection() {
	sl.event(InfoLevel, "new_connection").Msg("")
}

func (sl *SessionLogger) Disconnected() {
	sl.event(InfoLevel, "disconnected").Msg("")
}

func (sl *SessionLogger) MissingSettings(number, setting, detail string) {
	sl.event(WarnLevel, "missing_settings").
		Str("dialed_number", sanitizeField(number, 100)).
		Str("setting", sanitizeField(setting, 50)).
		Str("detail", sanitizeField(detail, 200)).
		Msg("")
}

// RateLimited logs a rate limit event (no session context available).
func (l *Logger) RateLimited(sourceIP, reason string) {
	l.Warn().
		Str("event", "rate_limited").
		Str("source_ip", sourceIP).
		Str("reason", reason).
		Msg("")
}
