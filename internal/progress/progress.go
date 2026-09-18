package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Reporter 进度报告器接口
type Reporter interface {
	Start(total int)
	Update(current int, msg string)
	Finish()
}

// ConsoleReporter 控制台进度报告器
type ConsoleReporter struct {
	total     int
	current   int
	startTime time.Time
	mu        sync.Mutex
	writer    io.Writer
	enabled   bool
}

// NewConsoleReporter 创建控制台进度报告器
func NewConsoleReporter() Reporter {
	return &ConsoleReporter{
		writer:  os.Stderr,
		enabled: true,
	}
}

// Start 开始进度报告
func (cr *ConsoleReporter) Start(total int) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.total = total
	cr.current = 0
	cr.startTime = time.Now()
}

// Update 更新进度
func (cr *ConsoleReporter) Update(current int, msg string) {
	if !cr.enabled {
		return
	}

	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.current = current
	cr.display(msg)
}

// Finish 完成进度报告
func (cr *ConsoleReporter) Finish() {
	if !cr.enabled {
		return
	}

	cr.mu.Lock()
	defer cr.mu.Unlock()
	cr.current = cr.total
	fmt.Fprintln(cr.writer) // 换行
}

// display 显示进度信息
func (cr *ConsoleReporter) display(msg string) {
	if cr.total == 0 {
		return
	}

	percentage := float64(cr.current) * 100 / float64(cr.total)
	elapsed := time.Since(cr.startTime)

	// 计算预估剩余时间
	var eta string
	if cr.current > 0 {
		avgTime := elapsed / time.Duration(cr.current)
		remaining := time.Duration(cr.total-cr.current) * avgTime
		eta = formatDuration(remaining)
	} else {
		eta = "calculating..."
	}

	// 构建进度条
	barWidth := 30
	filled := int(float64(barWidth) * percentage / 100)
	bar := strings.Repeat("=", filled) + strings.Repeat("-", barWidth-filled)

	// 清除当前行并显示进度
	output := fmt.Sprintf("\r[%s] %d/%d (%.1f%%) | %s | ETA: %s",
		bar, cr.current, cr.total, percentage, formatDuration(elapsed), eta)

	if msg != "" {
		output += " | " + msg
	}

	// 确保输出不会太长
	maxLen := 120
	if len(output) > maxLen {
		output = output[:maxLen-3] + "..."
	}

	fmt.Fprint(cr.writer, output)
}

// Progress 表示进度显示器
type Progress struct {
	title     string
	total     int
	current   int
	quiet     bool
	startTime time.Time
	mu        sync.Mutex
	writer    io.Writer
	enabled   bool
}

// NewProgress 创建新的进度显示器
func NewProgress(title string, total int, quiet bool) *Progress {
	return &Progress{
		title:     title,
		total:     total,
		current:   0,
		quiet:     quiet,
		startTime: time.Now(),
		writer:    os.Stderr,
		enabled:   !quiet && total > 0,
	}
}

// Update 更新进度
func (p *Progress) Update(msg string) {
	if !p.enabled {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current++
	p.display(msg)
}

// UpdateMsg 更新进度消息（不增加计数）
func (p *Progress) UpdateMsg(msg string) {
	if !p.enabled {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.display(msg)
}

// Finish 完成进度显示
func (p *Progress) Finish(msg string) {
	if !p.enabled {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.current = p.total
	p.display(msg)
	fmt.Fprintln(p.writer) // 换行
}

// display 显示进度信息
func (p *Progress) display(msg string) {
	if p.total == 0 {
		return
	}

	percentage := float64(p.current) * 100 / float64(p.total)
	elapsed := time.Since(p.startTime)

	// 计算预估剩余时间
	var eta string
	if p.current > 0 {
		avgTime := elapsed / time.Duration(p.current)
		remaining := time.Duration(p.total-p.current) * avgTime
		eta = formatDuration(remaining)
	} else {
		eta = "calculating..."
	}

	// 构建进度条
	barWidth := 30
	filled := int(float64(barWidth) * percentage / 100)
	bar := strings.Repeat("=", filled) + strings.Repeat("-", barWidth-filled)

	// 清除当前行并显示进度
	output := fmt.Sprintf("\r%s: [%s] %d/%d (%.1f%%) | %s | ETA: %s",
		p.title, bar, p.current, p.total, percentage, formatDuration(elapsed), eta)

	if msg != "" {
		output += " | " + msg
	}

	// 确保输出不会太长
	maxLen := 120
	if len(output) > maxLen {
		output = output[:maxLen-3] + "..."
	}

	fmt.Fprint(p.writer, output)
}

// formatDuration 格式化时间duration
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		minutes := int(d.Minutes())
		seconds := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%dm", hours, minutes)
}

// Inc 增加进度计数
func (p *Progress) Inc() {
	p.Update("")
}

// SetTotal 设置总数
func (p *Progress) SetTotal(total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total = total
	p.enabled = !p.quiet && total > 0
}

// GetCurrent 获取当前进度
func (p *Progress) GetCurrent() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current
}

// GetTotal 获取总数
func (p *Progress) GetTotal() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}
