package logger

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewDefaultLogger(t *testing.T) {
	log := NewDefaultLogger()
	if log == nil {
		t.Fatal("NewDefaultLogger returned nil")
	}
	if log.GetLevel() != LogLevelInfo {
		t.Errorf("Default level = %v, want Info", log.GetLevel())
	}
}

func TestSetLevel(t *testing.T) {
	log := NewDefaultLogger()
	log.SetLevel(LogLevelDebug)
	if log.GetLevel() != LogLevelDebug {
		t.Errorf("Level = %v, want Debug", log.GetLevel())
	}
	log.SetLevel(LogLevelError)
	if log.GetLevel() != LogLevelError {
		t.Errorf("Level = %v, want Error", log.GetLevel())
	}
}

func TestIsDebugEnabled(t *testing.T) {
	log := NewDefaultLogger()
	log.SetLevel(LogLevelDebug)
	if !log.IsDebugEnabled() {
		t.Error("IsDebugEnabled should be true at Debug level")
	}
	log.SetLevel(LogLevelError)
	if log.IsDebugEnabled() {
		t.Error("IsDebugEnabled should be false at Error level")
	}
}

func TestIsTraceEnabled(t *testing.T) {
	log := NewDefaultLogger()
	log.SetLevel(LogLevelTrace)
	if !log.IsTraceEnabled() {
		t.Error("IsTraceEnabled should be true at Trace level")
	}
	log.SetLevel(LogLevelInfo)
	if log.IsTraceEnabled() {
		t.Error("IsTraceEnabled should be false at Info level")
	}
}

func TestLogOutput(t *testing.T) {
	log := NewDefaultLogger()
	log.SetLevel(LogLevelInfo)

	// Redirect stdout to capture output
	var buf bytes.Buffer
	log.stdout = &buf

	log.Info("test message")
	output := buf.String()
	if !strings.Contains(output, "test message") {
		t.Errorf("Output should contain 'test message', got: %s", output)
	}
	if !strings.Contains(output, "[INFO]") {
		t.Errorf("Output should contain [INFO], got: %s", output)
	}
}

func TestLogLevelFiltering(t *testing.T) {
	log := NewDefaultLogger()
	log.SetLevel(LogLevelError)

	var stdoutBuf, stderrBuf bytes.Buffer
	log.stdout = &stdoutBuf
	log.stderr = &stderrBuf

	log.Info("should not appear")
	log.Error("should appear")

	stdoutOutput := stdoutBuf.String()
	stderrOutput := stderrBuf.String()
	if strings.Contains(stdoutOutput, "should not appear") || strings.Contains(stderrOutput, "should not appear") {
		t.Error("Info message should be filtered at Error level")
	}
	if !strings.Contains(stderrOutput, "should appear") {
		t.Error("Error message should be visible at Error level (stderr)")
	}
}

func TestSetTimestampEnabled(t *testing.T) {
	log := NewDefaultLogger()
	log.SetTimestampEnabled(false)

	var buf bytes.Buffer
	log.stdout = &buf
	log.SetLevel(LogLevelInfo)

	log.Info("test")
	output := buf.String()
	// Without timestamp, output should start with [INFO] not a date
	if strings.Contains(output, "2026") || strings.Contains(output, "2025") {
		t.Error("Timestamp should be disabled")
	}
}

func TestGetLevelString(t *testing.T) {
	tests := []struct {
		level LogLevel
		want  string
	}{
		{LogLevelError, "ERROR"},
		{LogLevelWarn, "WARN"},
		{LogLevelInfo, "INFO"},
		{LogLevelDebug, "DEBUG"},
		{LogLevelTrace, "TRACE"},
	}

	for _, tt := range tests {
		got := getLevelString(tt.level)
		if got != tt.want {
			t.Errorf("getLevelString(%v) = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestGlobalLogger(t *testing.T) {
	original := Global()
	defer func() { SetGlobalLogger(original) }()

	customLog := NewDefaultLogger()
	SetGlobalLogger(customLog)

	if Global() != customLog {
		t.Error("SetGlobalLogger should set Global")
	}
}

func TestGlobalLogFunctions(_ *testing.T) {
	// These should not panic
	Error("test error")
	Warn("test warn")
	Info("test info")
	Debug("test debug")
	Trace("test trace")
}

func TestSetLevelGlobal(_ *testing.T) {
	SetLevel(LogLevelError)
	// Should not panic
}

func TestFieldLogger(t *testing.T) {
	log := NewDefaultLogger()
	fl := log.WithFields(map[string]interface{}{"key": "value"})
	if fl == nil {
		t.Fatal("WithFields returned nil")
	}

	var buf bytes.Buffer
	log.stdout = &buf
	log.SetLevel(LogLevelInfo)

	fl.Info("test with fields")
	output := buf.String()
	if !strings.Contains(output, "test with fields") {
		t.Errorf("Output should contain message, got: %s", output)
	}
	if !strings.Contains(output, "key=value") {
		t.Errorf("Output should contain field, got: %s", output)
	}
}
