package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
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

		f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
		if err != nil {
			return nil, err
		}
		output = io.MultiWriter(os.Stdout, f)
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

func (l *Logger) ConnectionAttempt(sourceIP, dialedNumber, result string) {
	l.Info().
		Str("event", "connection_attempt").
		Str("source_ip", sourceIP).
		Str("dialed_number", dialedNumber).
		Str("result", result).
		Msg("")
}

func (l *Logger) InvalidCommand(sourceIP, command string) {
	// Truncate and strip non-printable characters to prevent log abuse
	if len(command) > 100 {
		command = command[:100]
	}
	sanitized := make([]byte, 0, len(command))
	for i := 0; i < len(command); i++ {
		if command[i] >= 32 && command[i] < 127 {
			sanitized = append(sanitized, command[i])
		}
	}
	l.Warn().
		Str("event", "invalid_command").
		Str("source_ip", sourceIP).
		Str("command", string(sanitized)).
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
		Str("dialed_number", number).
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

func (l *Logger) MissingInit(sourceIP, number, requiredInit string) {
	l.Warn().
		Str("event", "missing_init").
		Str("source_ip", sourceIP).
		Str("dialed_number", number).
		Str("required_init", requiredInit).
		Msg("")
}

func (l *Logger) MissingSettings(sourceIP, number, setting, detail string) {
	l.Warn().
		Str("event", "missing_settings").
		Str("source_ip", sourceIP).
		Str("dialed_number", number).
		Str("setting", setting).
		Str("detail", detail).
		Msg("")
}
