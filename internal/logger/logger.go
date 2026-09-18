package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LogLevel 日志级别
type LogLevel int

const (
	LogLevelError LogLevel = iota
	LogLevelWarn
	LogLevelInfo
	LogLevelDebug
	LogLevelTrace
)

// Logger 接口定义
type Logger interface {
	Error(format string, args ...interface{})
	Warn(format string, args ...interface{})
	Info(format string, args ...interface{})
	Debug(format string, args ...interface{})
	Trace(format string, args ...interface{})
	SetLevel(level LogLevel)
}

// DefaultLogger 默认日志实现
type DefaultLogger struct {
	level      LogLevel
	mu         sync.RWMutex // 使用读写锁提高并发性能
	stdout     io.Writer
	stderr     io.Writer // 错误输出
	debugFile  io.Writer
	debugMu    sync.Mutex
	debugLevel LogLevel
	timestamp  bool   // 是否包含时间戳
	callerInfo bool   // 是否包含调用者信息
	format     string // 时间格式
}

// NewDefaultLogger 创建默认日志记录器
func NewDefaultLogger() *DefaultLogger {
	return &DefaultLogger{
		level:      LogLevelInfo,
		stdout:     os.Stdout,
		stderr:     os.Stderr, // 默认错误输出到stderr
		debugLevel: LogLevelDebug,
		timestamp:  true,                  // 默认启用时间戳
		callerInfo: false,                 // 默认不启用调用者信息（性能考虑）
		format:     "2006-01-02 15:04:05", // 默认时间格式
	}
}

// SetDebugFile 设置调试日志文件
func (l *DefaultLogger) SetDebugFile(filename string) error {
	l.debugMu.Lock()
	defer l.debugMu.Unlock()

	if l.debugFile != nil {
		if closer, ok := l.debugFile.(io.Closer); ok {
			closer.Close()
		}
		l.debugFile = nil
	}

	if filename == "" {
		return nil
	}

	// 创建目录
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// 打开文件
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}

	l.debugFile = f
	return nil
}

// SetTimestampEnabled 设置是否启用时间戳
func (l *DefaultLogger) SetTimestampEnabled(enabled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.timestamp = enabled
}

// SetCallerInfoEnabled 设置是否启用调用者信息
func (l *DefaultLogger) SetCallerInfoEnabled(enabled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.callerInfo = enabled
}

// SetTimeFormat 设置时间格式
func (l *DefaultLogger) SetTimeFormat(format string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.format = format
}

// GetLevel 获取当前日志级别
func (l *DefaultLogger) GetLevel() LogLevel {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.level
}

// IsDebugEnabled 检查是否启用调试日志
func (l *DefaultLogger) IsDebugEnabled() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.level >= LogLevelDebug
}

// IsTraceEnabled 检查是否启用跟踪日志
func (l *DefaultLogger) IsTraceEnabled() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.level >= LogLevelTrace
}

// WithFields 添加字段信息（用于结构化日志）
func (l *DefaultLogger) WithFields(fields map[string]interface{}) *FieldLogger {
	return &FieldLogger{logger: l, fields: fields}
}

// SetLevel 设置日志级别
func (l *DefaultLogger) SetLevel(level LogLevel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// Error 输出错误日志
func (l *DefaultLogger) Error(format string, args ...interface{}) {
	l.log(LogLevelError, format, args...)
}

// Warn 输出警告日志
func (l *DefaultLogger) Warn(format string, args ...interface{}) {
	l.log(LogLevelWarn, format, args...)
}

// Info 输出信息日志
func (l *DefaultLogger) Info(format string, args ...interface{}) {
	l.log(LogLevelInfo, format, args...)
}

// Debug 输出调试日志
func (l *DefaultLogger) Debug(format string, args ...interface{}) {
	l.log(LogLevelDebug, format, args...)
}

// Trace 输出跟踪日志
func (l *DefaultLogger) Trace(format string, args ...interface{}) {
	l.log(LogLevelTrace, format, args...)
}

// log 内部日志记录方法
func (l *DefaultLogger) log(level LogLevel, format string, args ...interface{}) {
	l.mu.RLock() // 使用读锁提高并发性能
	currentLevel := l.level
	useStderr := level == LogLevelError
	outputWriter := l.stdout
	if useStderr {
		outputWriter = l.stderr
	}
	timestamp := l.timestamp
	formatStr := l.format
	l.mu.RUnlock()

	// 格式化消息
	msg := fmt.Sprintf(format, args...)
	var output string

	if timestamp {
		now := time.Now().Format(formatStr)
		levelStr := getLevelString(level)
		output = fmt.Sprintf("%s [%s] %s\n", now, levelStr, msg)
	} else {
		levelStr := getLevelString(level)
		output = fmt.Sprintf("[%s] %s\n", levelStr, msg)
	}

	// 输出到控制台
	if level <= currentLevel {
		fmt.Fprint(outputWriter, output)
	}

	// 输出到调试文件
	l.debugMu.Lock()
	defer l.debugMu.Unlock()
	if l.debugFile != nil && level <= l.debugLevel {
		fmt.Fprint(l.debugFile, output)
	}
}

// getLevelString 获取日志级别字符串
func getLevelString(level LogLevel) string {
	switch level {
	case LogLevelError:
		return "ERROR"
	case LogLevelWarn:
		return "WARN"
	case LogLevelInfo:
		return "INFO"
	case LogLevelDebug:
		return "DEBUG"
	case LogLevelTrace:
		return "TRACE"
	default:
		return "UNKNOWN"
	}
}

// FieldLogger 带字段的日志记录器
type FieldLogger struct {
	logger *DefaultLogger
	fields map[string]interface{}
}

// Error 输出带字段的错误日志
func (fl *FieldLogger) Error(format string, args ...interface{}) {
	fl.logger.Error(fl.formatWithFields(format, args...))
}

// Warn 输出带字段的警告日志
func (fl *FieldLogger) Warn(format string, args ...interface{}) {
	fl.logger.Warn(fl.formatWithFields(format, args...))
}

// Info 输出带字段的信息日志
func (fl *FieldLogger) Info(format string, args ...interface{}) {
	fl.logger.Info(fl.formatWithFields(format, args...))
}

// Debug 输出带字段的调试日志
func (fl *FieldLogger) Debug(format string, args ...interface{}) {
	fl.logger.Debug(fl.formatWithFields(format, args...))
}

// Trace 输出带字段的跟踪日志
func (fl *FieldLogger) Trace(format string, args ...interface{}) {
	fl.logger.Trace(fl.formatWithFields(format, args...))
}

// formatWithFields 格式化带字段的消息
func (fl *FieldLogger) formatWithFields(format string, args ...interface{}) string {
	formatted := fmt.Sprintf(format, args...)
	fieldStr := ""
	for k, v := range fl.fields {
		fieldStr += fmt.Sprintf(" [%s=%v]", k, v)
	}
	return formatted + fieldStr
}

// 全局日志记录器：使用读写锁保护，避免 SetGlobalLogger 与并发日志调用之间的数据竞争
var (
	globalMu sync.RWMutex
	global   Logger = NewDefaultLogger()
)

// Global 返回当前全局日志记录器。
// 注意：读取的是快照；历史代码直接暴露 var Global，为保证兼容保留只读访问。
func Global() Logger {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global
}

// SetGlobalLogger 设置全局日志记录器（线程安全）
func SetGlobalLogger(logger Logger) {
	globalMu.Lock()
	defer globalMu.Unlock()
	global = logger
}

// Error 全局错误日志
func Error(format string, args ...interface{}) {
	Global().Error(format, args...)
}

// Warn 全局警告日志
func Warn(format string, args ...interface{}) {
	Global().Warn(format, args...)
}

// Info 全局信息日志
func Info(format string, args ...interface{}) {
	Global().Info(format, args...)
}

// Debug 全局调试日志
func Debug(format string, args ...interface{}) {
	Global().Debug(format, args...)
}

// Trace 全局跟踪日志
func Trace(format string, args ...interface{}) {
	Global().Trace(format, args...)
}

// SetLevel 设置全局日志级别
func SetLevel(level LogLevel) {
	Global().SetLevel(level)
}
