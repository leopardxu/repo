package color

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Color 表示颜色类型
type Color int

const (
	// 前景色
	Reset Color = iota
	Black
	Red
	Green
	Yellow
	Blue
	Magenta
	Cyan
	White

	// 亮色
	BrightBlack
	BrightRed
	BrightGreen
	BrightYellow
	BrightBlue
	BrightMagenta
	BrightCyan
	BrightWhite
)

// colorMap 颜色代码映射
var colorMap = map[Color]string{
	Reset:         "\033[0m",
	Black:         "\033[30m",
	Red:           "\033[31m",
	Green:         "\033[32m",
	Yellow:        "\033[33m",
	Blue:          "\033[34m",
	Magenta:       "\033[35m",
	Cyan:          "\033[36m",
	White:         "\033[37m",
	BrightBlack:   "\033[90m",
	BrightRed:     "\033[91m",
	BrightGreen:   "\033[92m",
	BrightYellow:  "\033[93m",
	BrightBlue:    "\033[94m",
	BrightMagenta: "\033[95m",
	BrightCyan:    "\033[96m",
	BrightWhite:   "\033[97m",
}

// Coloring 表示颜色输出器
type Coloring struct {
	enabled bool
	writer  io.Writer
}

// NewColoring 创建颜色输出器
func NewColoring(enabled bool) *Coloring {
	return &Coloring{
		enabled: enabled,
		writer:  os.Stdout,
	}
}

// SetWriter 设置输出目标
func (c *Coloring) SetWriter(w io.Writer) {
	c.writer = w
}

// SetEnabled 设置是否启用颜色
func (c *Coloring) SetEnabled(enabled bool) {
	c.enabled = enabled
}

// IsEnabled 返回是否启用颜色
func (c *Coloring) IsEnabled() bool {
	return c.enabled
}

// Colorize 给文本添加颜色
func (c *Coloring) Colorize(text string, color Color) string {
	if !c.enabled {
		return text
	}
	return colorMap[color] + text + colorMap[Reset]
}

// Print 打印彩色文本
func (c *Coloring) Print(text string, color Color) {
	fmt.Fprint(c.writer, c.Colorize(text, color))
}

// Println 打印彩色文本并换行
func (c *Coloring) Println(text string, color Color) {
	fmt.Fprintln(c.writer, c.Colorize(text, color))
}

// Printf 格式化打印彩色文本
func (c *Coloring) Printf(format string, color Color, args ...interface{}) {
	text := fmt.Sprintf(format, args...)
	fmt.Fprint(c.writer, c.Colorize(text, color))
}

// 便捷方法
func (c *Coloring) Red(text string) string {
	return c.Colorize(text, Red)
}

func (c *Coloring) Green(text string) string {
	return c.Colorize(text, Green)
}

func (c *Coloring) Yellow(text string) string {
	return c.Colorize(text, Yellow)
}

func (c *Coloring) Blue(text string) string {
	return c.Colorize(text, Blue)
}

func (c *Coloring) Cyan(text string) string {
	return c.Colorize(text, Cyan)
}

func (c *Coloring) Magenta(text string) string {
	return c.Colorize(text, Magenta)
}

// BranchColoring 分支颜色输出器
type BranchColoring struct {
	*Coloring
}

// NewBranchColoring 创建分支颜色输出器
func NewBranchColoring(enabled bool) *BranchColoring {
	return &BranchColoring{
		Coloring: NewColoring(enabled),
	}
}

// Current 当前分支颜色（绿色）
func (bc *BranchColoring) Current(text string) string {
	return bc.Green(text)
}

// Local 本地分支颜色（默认）
func (bc *BranchColoring) Local(text string) string {
	if !bc.enabled {
		return text
	}
	return text
}

// NotInProject 不在项目中的分支颜色（红色）
func (bc *BranchColoring) NotInProject(text string) string {
	return bc.Red(text)
}

// Published 已发布的分支颜色（绿色）
func (bc *BranchColoring) Published(text string) string {
	return bc.Green(text)
}

// ShouldUseColor 检测是否应该使用颜色
func ShouldUseColor(colorOption string) bool {
	switch strings.ToLower(colorOption) {
	case "always":
		return true
	case "never":
		return false
	case "auto":
		// 检测是否是终端；Stat 失败（stdout 已关闭等）时按非终端处理
		// 避免 fileInfo 为 nil 时的空指针解引用 panic
		fileInfo, err := os.Stdout.Stat()
		if err != nil {
			return false
		}
		return (fileInfo.Mode() & os.ModeCharDevice) != 0
	default:
		return false
	}
}
