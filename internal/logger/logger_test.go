package logger

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestNewDefaultLogger(t *testing.T) {
	// 级别是进程级共享状态，先归位避免受其它测试影响。
	SetLevel(LogLevelInfo)
	t.Cleanup(func() { SetLevel(LogLevelInfo) })

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

	// 捕获非错误日志输出
	var buf bytes.Buffer
	log.SetOutput(&buf)

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
	log.SetOutput(&stdoutBuf)
	log.errOutput = &stderrBuf

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
	log.SetOutput(&buf)
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
	log.SetOutput(&buf)
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

func TestFieldLoggerPreservesPercentInMessage(t *testing.T) {
	tests := []struct {
		name   string
		render func(fl *FieldLogger)
	}{
		{name: "Error", render: func(fl *FieldLogger) { fl.Error("progress 100%% %s", "done") }},
		{name: "Warn", render: func(fl *FieldLogger) { fl.Warn("progress 100%% %s", "done") }},
		{name: "Info", render: func(fl *FieldLogger) { fl.Info("progress 100%% %s", "done") }},
		{name: "Debug", render: func(fl *FieldLogger) { fl.Debug("progress 100%% %s", "done") }},
		{name: "Trace", render: func(fl *FieldLogger) { fl.Trace("progress 100%% %s", "done") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := NewDefaultLogger()
			log.SetLevel(LogLevelTrace)
			var buf bytes.Buffer
			log.SetOutput(&buf)
			log.errOutput = &buf

			tt.render(log.WithFields(map[string]interface{}{"key": "value"}))

			got := buf.String()
			// 组合后的文本只做一次 Sprintf，% 不得被二次解释
			if !strings.Contains(got, "progress 100% done") {
				t.Errorf("消息被二次格式化, got %q", got)
			}
			if strings.Contains(got, "%!") {
				t.Errorf("输出含 fmt 错误标记, got %q", got)
			}
			// fields 输出格式保持不变
			if !strings.Contains(got, "[key=value]") {
				t.Errorf("字段输出格式丢失, got %q", got)
			}
		})
	}
}

func TestDefaultLoggerOutputIsStderr(t *testing.T) {
	log := NewDefaultLogger()
	if log.output != os.Stderr {
		t.Errorf("非错误日志默认输出 = %v, want os.Stderr", log.output)
	}
	if log.errOutput != os.Stderr {
		t.Errorf("错误日志默认输出 = %v, want os.Stderr", log.errOutput)
	}
}

func TestSetOutputOnlyAffectsNonErrorLogs(t *testing.T) {
	log := NewDefaultLogger()
	log.SetLevel(LogLevelInfo)

	var infoBuf, errBuf bytes.Buffer
	log.SetOutput(&infoBuf)
	log.errOutput = &errBuf

	log.Info("info-line")
	log.Error("error-line")

	if !strings.Contains(infoBuf.String(), "info-line") {
		t.Errorf("非错误日志应写入 SetOutput 目标, got %q", infoBuf.String())
	}
	if strings.Contains(infoBuf.String(), "error-line") {
		t.Error("错误日志不应写入非错误输出")
	}
	if !strings.Contains(errBuf.String(), "error-line") {
		t.Errorf("错误日志应写入 stderr, got %q", errBuf.String())
	}
}

func TestSetOutputNilDiscards(t *testing.T) {
	log := NewDefaultLogger()
	log.SetOutput(nil)
	log.SetLevel(LogLevelInfo)

	if log.output != io.Discard {
		t.Errorf("SetOutput(nil) 后输出 = %v, want io.Discard", log.output)
	}
	// 不应 panic，也不应写 stderr
	log.Info("discarded")
}

// TestSetLevelAffectsAllLoggers 验证日志级别是进程级状态：
// 命令在自身实例上设置级别后，包级 logger 也立即生效。
func TestSetLevelAffectsAllLoggers(t *testing.T) {
	SetLevel(LogLevelInfo)
	t.Cleanup(func() { SetLevel(LogLevelInfo) })

	packageLog := NewDefaultLogger() // 模拟 internal 包的包级 logger
	commandLog := NewDefaultLogger() // 模拟命令级 logger
	var buf bytes.Buffer
	packageLog.SetOutput(&buf)

	commandLog.SetLevel(LogLevelError) // -q
	if got := packageLog.GetLevel(); got != LogLevelError {
		t.Fatalf("包级 logger 级别 = %v, want Error", got)
	}
	packageLog.Info("quiet-info-suppressed")
	if strings.Contains(buf.String(), "quiet-info-suppressed") {
		t.Errorf("-q 级别下包级 logger 仍输出 Info: %q", buf.String())
	}

	commandLog.SetLevel(LogLevelDebug) // -v
	packageLog.Debug("verbose-debug-visible")
	if !strings.Contains(buf.String(), "verbose-debug-visible") {
		t.Errorf("-v 级别下包级 logger 未输出 Debug: %q", buf.String())
	}
}

// TestQuietLevelSuppressesInfo 验证 -q（Error 级别）抑制 Info/Warn/Debug。
func TestQuietLevelSuppressesInfo(t *testing.T) {
	SetLevel(LogLevelInfo)
	t.Cleanup(func() { SetLevel(LogLevelInfo) })

	log := NewDefaultLogger()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetLevel(LogLevelError)

	log.Info("info-hidden")
	log.Debug("debug-hidden")
	log.Warn("warn-hidden")

	if buf.Len() != 0 {
		t.Errorf("-q 下非错误日志不应输出，got %q", buf.String())
	}
}

// TestVerboseLevelEnablesDebug 验证 -v（Debug 级别）打开 Debug 输出。
func TestVerboseLevelEnablesDebug(t *testing.T) {
	SetLevel(LogLevelInfo)
	t.Cleanup(func() { SetLevel(LogLevelInfo) })

	log := NewDefaultLogger()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetLevel(LogLevelDebug)

	if !log.IsDebugEnabled() {
		t.Error("Debug 级别下 IsDebugEnabled() 应为 true")
	}
	log.Debug("debug-visible")
	if !strings.Contains(buf.String(), "debug-visible") {
		t.Errorf("-v 下 Debug 日志未输出: %q", buf.String())
	}
	if strings.Contains(buf.String(), "[TRACE]") {
		t.Errorf("Debug 级别不应输出 Trace: %q", buf.String())
	}
}
