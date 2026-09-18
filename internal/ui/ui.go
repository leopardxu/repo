package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/leopardxu/repo-go/internal/color"
)

// OutputType 输出类型
type OutputType int

const (
	// 普通输出
	OutputNormal OutputType = iota
	// 成功输出
	OutputSuccess
	// 警告输出
	OutputWarning
	// 错误输出
	OutputError
	// 信息输出
	OutputInfo
	// 调试输出
	OutputDebug
)

// UI 提供用户界面功能
type UI struct {
	colorize bool
	verbose  bool
	quiet    bool
	coloring *color.Coloring
}

// NewUI 创建新的UI实例
func NewUI() *UI {
	ui := &UI{
		colorize: true,
		verbose:  false,
		quiet:    false,
	}

	// 检查是否应该禁用颜色
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		ui.colorize = false
	}

	// 检查环境变量设置
	if verbose := os.Getenv("REPO_VERBOSE"); verbose == "true" || verbose == "1" {
		ui.verbose = true
	}

	if quiet := os.Getenv("REPO_QUIET"); quiet == "true" || quiet == "1" {
		ui.quiet = true
	}

	ui.coloring = color.NewColoring(ui.colorize)

	return ui
}

// SetColorized 设置是否启用颜色输出
func (ui *UI) SetColorized(colorized bool) {
	ui.colorize = colorized
	ui.coloring.SetEnabled(colorized)
}

// SetVerbose 设置详细输出模式
func (ui *UI) SetVerbose(verbose bool) {
	ui.verbose = verbose
}

// SetQuiet 设置安静模式
func (ui *UI) SetQuiet(quiet bool) {
	ui.quiet = quiet
}

// Print 输出普通文本
func (ui *UI) Print(args ...interface{}) {
	if ui.quiet {
		return
	}
	fmt.Print(args...)
}

// Println 输出普通文本并换行
func (ui *UI) Println(args ...interface{}) {
	if ui.quiet {
		return
	}
	fmt.Println(args...)
}

// Printf 格式化输出普通文本
func (ui *UI) Printf(format string, args ...interface{}) {
	if ui.quiet {
		return
	}
	fmt.Printf(format, args...)
}

// Output 输出指定类型的文本
func (ui *UI) Output(outputType OutputType, args ...interface{}) {
	if ui.quiet && outputType != OutputError {
		return
	}

	text := fmt.Sprint(args...)

	switch outputType {
	case OutputSuccess:
		ui.printSuccess(text)
	case OutputWarning:
		ui.printWarning(text)
	case OutputError:
		ui.printError(text)
	case OutputInfo:
		ui.printInfo(text)
	case OutputDebug:
		if ui.verbose {
			ui.printDebug(text)
		}
	default:
		fmt.Print(args...)
	}
}

// Outputf 格式化输出指定类型的文本
func (ui *UI) Outputf(outputType OutputType, format string, args ...interface{}) {
	if ui.quiet && outputType != OutputError {
		return
	}

	text := fmt.Sprintf(format, args...)

	switch outputType {
	case OutputSuccess:
		ui.printSuccess(text)
	case OutputWarning:
		ui.printWarning(text)
	case OutputError:
		ui.printError(text)
	case OutputInfo:
		ui.printInfo(text)
	case OutputDebug:
		if ui.verbose {
			ui.printDebug(text)
		}
	default:
		fmt.Printf(format, args...)
	}
}

// Success 输出成功信息
func (ui *UI) Success(args ...interface{}) {
	if ui.quiet {
		return
	}
	ui.printSuccess(fmt.Sprint(args...))
}

// Successf 格式化输出成功信息
func (ui *UI) Successf(format string, args ...interface{}) {
	if ui.quiet {
		return
	}
	ui.printSuccess(fmt.Sprintf(format, args...))
}

// Warning 输出警告信息
func (ui *UI) Warning(args ...interface{}) {
	if ui.quiet {
		return
	}
	ui.printWarning(fmt.Sprint(args...))
}

// Warningf 格式化输出警告信息
func (ui *UI) Warningf(format string, args ...interface{}) {
	if ui.quiet {
		return
	}
	ui.printWarning(fmt.Sprintf(format, args...))
}

// Error 输出错误信息
func (ui *UI) Error(args ...interface{}) {
	ui.printError(fmt.Sprint(args...))
}

// Errorf 格式化输出错误信息
func (ui *UI) Errorf(format string, args ...interface{}) {
	ui.printError(fmt.Sprintf(format, args...))
}

// Info 输出信息
func (ui *UI) Info(args ...interface{}) {
	if ui.quiet {
		return
	}
	ui.printInfo(fmt.Sprint(args...))
}

// Infof 格式化输出信息
func (ui *UI) Infof(format string, args ...interface{}) {
	if ui.quiet {
		return
	}
	ui.printInfo(fmt.Sprintf(format, args...))
}

// Debug 输出调试信息
func (ui *UI) Debug(args ...interface{}) {
	if ui.quiet || !ui.verbose {
		return
	}
	ui.printDebug(fmt.Sprint(args...))
}

// Debugf 格式化输出调试信息
func (ui *UI) Debugf(format string, args ...interface{}) {
	if ui.quiet || !ui.verbose {
		return
	}
	ui.printDebug(fmt.Sprintf(format, args...))
}

// printSuccess 输出成功信息
func (ui *UI) printSuccess(text string) {
	if ui.colorize {
		fmt.Print(ui.coloring.Green("[SUCCESS] "))
		fmt.Println(text)
	} else {
		fmt.Println("[SUCCESS]", text)
	}
}

// printWarning 输出警告信息
func (ui *UI) printWarning(text string) {
	if ui.colorize {
		fmt.Print(ui.coloring.Yellow("[WARNING] "))
		fmt.Println(text)
	} else {
		fmt.Println("[WARNING]", text)
	}
}

// printError 输出错误信息
func (ui *UI) printError(text string) {
	if ui.colorize {
		fmt.Fprint(os.Stderr, ui.coloring.Red("[ERROR] "))
		fmt.Fprintln(os.Stderr, text)
	} else {
		fmt.Fprintln(os.Stderr, "[ERROR]", text)
	}
}

// printInfo 输出信息
func (ui *UI) printInfo(text string) {
	if ui.colorize {
		fmt.Print(ui.coloring.Blue("[INFO] "))
		fmt.Println(text)
	} else {
		fmt.Println("[INFO]", text)
	}
}

// printDebug 输出调试信息
func (ui *UI) printDebug(text string) {
	if ui.colorize {
		fmt.Print(ui.coloring.Cyan("[DEBUG] "))
		fmt.Println(text)
	} else {
		fmt.Println("[DEBUG]", text)
	}
}

// ProgressBar 显示进度条
func (ui *UI) ProgressBar(current, total int, prefix, suffix string) {
	if ui.quiet {
		return
	}

	width := 40
	if total <= 0 {
		return
	}

	percent := float64(current) / float64(total)
	completed := int(percent * float64(width))

	var bar string
	bar += strings.Repeat("█", completed)
	bar += strings.Repeat("░", width-completed)

	progress := fmt.Sprintf("%.1f%%", percent*100)
	output := fmt.Sprintf("\r%s |%s| %s %d/%d %s", prefix, bar, progress, current, total, suffix)

	fmt.Print(output)

	if current == total {
		fmt.Println() // 换行
	}
}

// Spinner 显示旋转指示器
type Spinner struct {
	ui      *UI
	message string
	stop    chan bool
	ticker  *time.Ticker
}

// StartSpinner 启动旋转指示器
func (ui *UI) StartSpinner(message string) *Spinner {
	if ui.quiet {
		return &Spinner{ui: ui, message: message}
	}

	spinner := &Spinner{
		ui:      ui,
		message: message,
		stop:    make(chan bool),
		ticker:  time.NewTicker(100 * time.Millisecond),
	}

	go func() {
		chars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		i := 0

		for {
			select {
			case <-spinner.stop:
				spinner.ticker.Stop()
				return
			case <-spinner.ticker.C:
				var coloredMsg string
				if ui.colorize {
					coloredMsg = ui.coloring.Cyan(chars[i%len(chars)]) + " " + ui.coloring.Blue(spinner.message)
				} else {
					coloredMsg = chars[i%len(chars)] + " " + spinner.message
				}
				fmt.Printf("\r%s %s", coloredMsg, "\033[K")
				i++
			}
		}
	}()

	return spinner
}

// StopSpinner 停止旋转指示器
func (s *Spinner) Stop() {
	if s.stop != nil {
		s.stop <- true
		close(s.stop)

		// 清除旋转指示器
		fmt.Print("\033[2K\r") // 清除整行
	}
}

// Confirmation 请求用户确认
func (ui *UI) Confirmation(prompt string) bool {
	if ui.quiet {
		return true // 安静模式下默认确认
	}

	fmt.Printf("%s [y/N]: ", prompt)
	var response string
	// Scanln 出错（如 EOF/空输入）时 response 保持零值，按未输入处理即默认拒绝
	_, _ = fmt.Scanln(&response)

	response = strings.ToLower(strings.TrimSpace(response))
	return response == "y" || response == "yes"
}

// GetInput 获取用户输入
func (ui *UI) GetInput(prompt string) string {
	if ui.quiet {
		return ""
	}

	fmt.Print(prompt + ": ")
	var input string
	// Scanln 出错（如 EOF/空输入）时 input 保持零值，返回空串交由调用方处理
	_, _ = fmt.Scanln(&input)
	return strings.TrimSpace(input)
}

// GlobalUI 全局UI实例
var GlobalUI = NewUI()

// 全局函数
func Print(args ...interface{})                         { GlobalUI.Print(args...) }
func Println(args ...interface{})                       { GlobalUI.Println(args...) }
func Printf(format string, args ...interface{})         { GlobalUI.Printf(format, args...) }
func Output(outputType OutputType, args ...interface{}) { GlobalUI.Output(outputType, args...) }
func Outputf(outputType OutputType, format string, args ...interface{}) {
	GlobalUI.Outputf(outputType, format, args...)
}
func Success(args ...interface{})                 { GlobalUI.Success(args...) }
func Successf(format string, args ...interface{}) { GlobalUI.Successf(format, args...) }
func Warning(args ...interface{})                 { GlobalUI.Warning(args...) }
func Warningf(format string, args ...interface{}) { GlobalUI.Warningf(format, args...) }
func Error(args ...interface{})                   { GlobalUI.Error(args...) }
func Errorf(format string, args ...interface{})   { GlobalUI.Errorf(format, args...) }
func Info(args ...interface{})                    { GlobalUI.Info(args...) }
func Infof(format string, args ...interface{})    { GlobalUI.Infof(format, args...) }
func Debug(args ...interface{})                   { GlobalUI.Debug(args...) }
func Debugf(format string, args ...interface{})   { GlobalUI.Debugf(format, args...) }
