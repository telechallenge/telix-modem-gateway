package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
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
	level  Level
	json   bool
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

	return &Logger{
		output: output,
		level:  ParseLevel(level),
		json:   format == "json",
	}, nil
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
	if e.level < e.logger.level {
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

func (l *Logger) ConnectionAttempt(sourceIP, dialedNumber, result string) {
	l.Info().
		Str("event", "connection_attempt").
		Str("source_ip", sourceIP).
		Str("dialed_number", sanitizeField(dialedNumber, 100)).
		Str("result", result).
		Msg("")
}

func (l *Logger) InvalidCommand(sourceIP, command string) {
	l.Warn().
		Str("event", "invalid_command").
		Str("source_ip", sourceIP).
		Str("command", sanitizeField(command, 100)).
		Msg("")
}

func (l *Logger) RateLimited(sourceIP, reason string) {
	l.Warn().
		Str("event", "rate_limited").
		Str("source_ip", sourceIP).
		Str("reason", reason).
		Msg("")
}

func (l *Logger) InvalidNumber(sourceIP, number string) {
	l.Warn().
		Str("event", "invalid_number").
		Str("source_ip", sourceIP).
		Str("dialed_number", sanitizeField(number, 100)).
		Msg("")
}

func (l *Logger) NewConnection(sourceIP string) {
	l.Info().
		Str("event", "new_connection").
		Str("source_ip", sourceIP).
		Msg("")
}

func (l *Logger) Disconnected(sourceIP string) {
	l.Info().
		Str("event", "disconnected").
		Str("source_ip", sourceIP).
		Msg("")
}

func (l *Logger) MissingSettings(sourceIP, number, setting, detail string) {
	l.Warn().
		Str("event", "missing_settings").
		Str("source_ip", sourceIP).
		Str("dialed_number", sanitizeField(number, 100)).
		Str("setting", sanitizeField(setting, 50)).
		Str("detail", sanitizeField(detail, 200)).
		Msg("")
}
