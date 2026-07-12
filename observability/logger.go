package observability

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// LogLevel defines the severity of a log entry.
type LogLevel string

const (
	LevelInfo  LogLevel = "INFO"
	LevelWarn  LogLevel = "WARN"
	LevelError LogLevel = "ERROR"
	LevelDebug LogLevel = "DEBUG"
)

// LogEntry represents a single structured log line.
type LogEntry struct {
	Timestamp time.Time              `json:"timestamp"`
	Level     LogLevel               `json:"level"`
	Message   string                 `json:"message"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

// Logger provides JSON structured logging for DarkCode.
type Logger struct {
	mu      sync.Mutex
	file    *os.File
	console bool // mirror human-readable lines to stdout/stderr (CLI mode)
}

var GlobalLogger *Logger
var disabledLogger = &Logger{}

// Logger returns the process-wide structured logger, or a safe no-op logger
// if InitLogger has not run / failed. Callers can therefore always call
// Logger().Info(...) without nil checks.
func Log() *Logger {
	if GlobalLogger != nil {
		return GlobalLogger
	}
	return disabledLogger
}

// InitLogger opens (or appends to) darkcode.log and stores it as the global
// logger. When console is true (CLI mode), human-readable lines are also
// mirrored to stdout/stderr. A failure to open the file is non-fatal: the
// logger still mirrors to the console.
func InitLogger(console bool) {
	l, err := NewLogger("darkcode.log")
	if err != nil {
		// Fall back to a console-only logger so boot diagnostics are not lost.
		GlobalLogger = &Logger{console: console}
		return
	}
	l.console = console
	GlobalLogger = l
}

func NewLogger(logPath string) (*Logger, error) {
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &Logger{file: file}, nil
}

func (l *Logger) log(level LogLevel, msg string, fields map[string]interface{}) {
	if l == nil {
		return
	}
	entry := LogEntry{
		Timestamp: time.Now(),
		Level:     level,
		Message:   msg,
		Fields:    fields,
	}
	if l.file != nil {
		data, _ := json.Marshal(entry)
		l.mu.Lock()
		l.file.Write(append(data, '\n'))
		l.mu.Unlock()
	}
	if l.console {
		stream := os.Stdout
		if level == LevelError || level == LevelWarn {
			stream = os.Stderr
		}
		if len(fields) > 0 {
			fmt.Fprintf(stream, "[%s] %s %v\n", level, msg, fields)
		} else {
			fmt.Fprintf(stream, "[%s] %s\n", level, msg)
		}
	}
}

func (l *Logger) Info(msg string, fields map[string]interface{}) {
	l.log(LevelInfo, msg, fields)
}

func (l *Logger) Warn(msg string, fields map[string]interface{}) {
	l.log(LevelWarn, msg, fields)
}

func (l *Logger) Error(msg string, err error, fields map[string]interface{}) {
	if fields == nil {
		fields = make(map[string]interface{})
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	l.log(LevelError, msg, fields)
}

func (l *Logger) Close() {
	if l != nil && l.file != nil {
		l.file.Close()
	}
}

// Metrics handles basic performance monitoring.
type Metrics struct {
	counters map[string]int64
}

func NewMetrics() *Metrics {
	return &Metrics{
		counters: make(map[string]int64),
	}
}

func (m *Metrics) Increment(key string) {
	m.counters[key]++
}

func (m *Metrics) Get(key string) int64 {
	return m.counters[key]
}
