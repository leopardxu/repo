package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// progressBarWidth 进度条固定宽度
const progressBarWidth = 30

// maxDisplayLen 单行进度输出的最大字节数；超出时截断并追加 "..."。
const maxDisplayLen = 120

// truncateUTF8 将 s 截断到最多 maxLen 字节，但不会切断 UTF-8 多字节字符：
// 截断点落在字符中间时向前回退到最近的 rune 起始字节，保证输出仍是合法 UTF-8。
// 需要截断时尾部追加 "..."（计入 maxLen）。
func truncateUTF8(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}

	limit := maxLen - len("...")
	if limit <= 0 {
		// maxLen 连 "..." 都放不下：退化为纯截断，仍保证不切坏字符
		limit = maxLen
		for limit > 0 && !utf8.RuneStart(s[limit]) {
			limit--
		}
		return s[:limit]
	}
	// s[limit] 若是续字节（非 rune 起始字节），说明切点落在多字节字符内部，向前回退
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + "..."
}

// clampCurrent 将当前进度收敛到 [0, total]；total 非正时统一返回 0，
// 避免后续除零或负数计算。
func clampCurrent(current, total int) int {
	if total <= 0 || current < 0 {
		return 0
	}
	if current > total {
		return total
	}
	return current
}

// barFilled 计算进度条填充格数，结果始终位于 [0, barWidth]，
// 不会因 current 越界导致 strings.Repeat 收到负数而 panic。
func barFilled(current, total, barWidth int) int {
	if total <= 0 || barWidth <= 0 {
		return 0
	}
	current = clampCurrent(current, total)
	filled := int(int64(barWidth) * int64(current) / int64(total))
	if filled < 0 {
		return 0
	}
	if filled > barWidth {
		return barWidth
	}
	return filled
}

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

// Update 更新进度。
// 采用绝对计数语义：current 表示累计完成数，而非增量；调用方每次都传入当前累计值。
// 越界值会被收敛到 [0, total]，因此 Finish 之后再次调用也不会 panic。
func (cr *ConsoleReporter) Update(current int, msg string) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if !cr.enabled {
		return
	}
	cr.current = clampCurrent(current, cr.total)
	cr.display(msg)
}

// Finish 完成进度报告
func (cr *ConsoleReporter) Finish() {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if !cr.enabled {
		return
	}
	cr.current = clampCurrent(cr.total, cr.total)
	fmt.Fprintln(cr.writer) // 换行
}

// display 显示进度信息
func (cr *ConsoleReporter) display(msg string) {
	if cr.total <= 0 {
		return
	}

	current := clampCurrent(cr.current, cr.total)
	percentage := float64(current) * 100 / float64(cr.total)
	elapsed := time.Since(cr.startTime)

	// 计算预估剩余时间
	var eta string
	if current > 0 {
		avgTime := elapsed / time.Duration(current)
		remaining := time.Duration(cr.total-current) * avgTime
		eta = formatDuration(remaining)
	} else {
		eta = "calculating..."
	}

	// 构建进度条（filled 已保证在 [0, progressBarWidth]）
	filled := barFilled(current, cr.total, progressBarWidth)
	bar := strings.Repeat("=", filled) + strings.Repeat("-", progressBarWidth-filled)

	// 清除当前行并显示进度
	output := fmt.Sprintf("\r[%s] %d/%d (%.1f%%) | %s | ETA: %s",
		bar, current, cr.total, percentage, formatDuration(elapsed), eta)

	if msg != "" {
		output += " | " + msg
	}

	// 确保输出不会太长；按 rune 边界截断，避免切坏中文等多字节字符
	output = truncateUTF8(output, maxDisplayLen)

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

// Update 更新进度。
// 该方法按增量计数（内部 current+1），与 Reporter.Update 的绝对计数语义不同；
// 计数会被收敛到 [0, total]，Finish 之后继续调用也不会 panic。
func (p *Progress) Update(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled {
		return
	}

	p.current = clampCurrent(p.current+1, p.total)
	p.display(msg)
}

// UpdateMsg 更新进度消息（不增加计数）
func (p *Progress) UpdateMsg(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled {
		return
	}

	p.display(msg)
}

// Finish 完成进度显示
func (p *Progress) Finish(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled {
		return
	}

	p.current = clampCurrent(p.total, p.total)
	p.display(msg)
	fmt.Fprintln(p.writer) // 换行
}

// display 显示进度信息
func (p *Progress) display(msg string) {
	if p.total <= 0 {
		return
	}

	current := clampCurrent(p.current, p.total)
	percentage := float64(current) * 100 / float64(p.total)
	elapsed := time.Since(p.startTime)

	// 计算预估剩余时间
	var eta string
	if current > 0 {
		avgTime := elapsed / time.Duration(current)
		remaining := time.Duration(p.total-current) * avgTime
		eta = formatDuration(remaining)
	} else {
		eta = "calculating..."
	}

	// 构建进度条（filled 已保证在 [0, progressBarWidth]）
	filled := barFilled(current, p.total, progressBarWidth)
	bar := strings.Repeat("=", filled) + strings.Repeat("-", progressBarWidth-filled)

	// 清除当前行并显示进度
	output := fmt.Sprintf("\r%s: [%s] %d/%d (%.1f%%) | %s | ETA: %s",
		p.title, bar, current, p.total, percentage, formatDuration(elapsed), eta)

	if msg != "" {
		output += " | " + msg
	}

	// 确保输出不会太长；按 rune 边界截断，避免切坏中文等多字节字符
	output = truncateUTF8(output, maxDisplayLen)

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
	p.current = clampCurrent(p.current, total)
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
