package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type LogConfig struct {
	Level     string
	FileLevel string
	DataDir   string
	MaxBuffer int
}

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

type Logger struct {
	mu           sync.Mutex
	consoleLevel int
	fileLevel    int
	maxBuffer    int
	filePath     string
	file         *os.File
	buffer       []LogEntry
}

var logLevels = map[string]int{
	"debug": 10,
	"info":  20,
	"warn":  30,
	"error": 40,
}

func NewLogger(cfg LogConfig) *Logger {
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	_ = os.MkdirAll(dataDir, 0o755)

	filePath := filepath.Join(dataDir, "emby-in-one.log")
	file, _ := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)

	maxBuffer := cfg.MaxBuffer
	if maxBuffer <= 0 {
		maxBuffer = 500
	}

	return &Logger{
		consoleLevel: levelValue(envOrDefault("LOG_LEVEL", cfg.Level, "info")),
		fileLevel:    levelValue(envOrDefault("FILE_LOG_LEVEL", cfg.FileLevel, "info")),
		maxBuffer:    maxBuffer,
		filePath:     filePath,
		file:         file,
		buffer:       make([]LogEntry, 0, maxBuffer),
	}
}

func defaultDataDir() string {
	if _, err := os.Stat("/app/data"); err == nil {
		return "/app/data"
	}
	return filepath.Clean(filepath.Join("data"))
}

func envOrDefault(envKey, configured, fallback string) string {
	if envValue := os.Getenv(envKey); envValue != "" {
		return envValue
	}
	if configured != "" {
		return configured
	}
	return fallback
}

func levelValue(level string) int {
	if v, ok := logLevels[strings.ToLower(level)]; ok {
		return v
	}
	return logLevels["info"]
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

func (l *Logger) log(level, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	level = strings.ToLower(level)
	entry := LogEntry{
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
		Level:     level,
		Message:   message,
	}
	l.buffer = append(l.buffer, entry)
	if len(l.buffer) > l.maxBuffer {
		l.buffer = append([]LogEntry(nil), l.buffer[len(l.buffer)-l.maxBuffer:]...)
	}

	line := fmt.Sprintf("%s [%s] %s\n", entry.Timestamp, strings.ToUpper(level), message)
	if levelValue(level) >= l.consoleLevel {
		_, _ = os.Stdout.WriteString(line)
	}
	if l.file != nil && levelValue(level) >= l.fileLevel {
		_, _ = l.file.WriteString(line)
	}
}

func (l *Logger) Debugf(format string, args ...any) { l.log("debug", fmt.Sprintf(format, args...)) }
func (l *Logger) Infof(format string, args ...any)  { l.log("info", fmt.Sprintf(format, args...)) }
func (l *Logger) Warnf(format string, args ...any)  { l.log("warn", fmt.Sprintf(format, args...)) }
func (l *Logger) Errorf(format string, args ...any) { l.log("error", fmt.Sprintf(format, args...)) }

func (l *Logger) Entries(limit int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.buffer) {
		limit = len(l.buffer)
	}
	start := len(l.buffer) - limit
	out := make([]LogEntry, limit)
	copy(out, l.buffer[start:])
	return out
}

func (l *Logger) FilePath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.filePath
}

func (l *Logger) ClearFile() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
	}
	if err := os.WriteFile(l.filePath, []byte{}, 0o644); err != nil {
		return err
	}
	file, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.file = file
	l.buffer = l.buffer[:0]
	return nil
}
