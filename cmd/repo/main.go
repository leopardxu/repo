package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/leopardxu/repo-go/cmd/repo/commands"
	"github.com/leopardxu/repo-go/internal/config"
	"github.com/leopardxu/repo-go/internal/git"
	"github.com/leopardxu/repo-go/internal/hook"
	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/spf13/cobra"
)

var (
	// 版本信息，将在构建时注入
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// errHelpShown 表示无子命令时的帮助已经打印；main 据此以非零码退出
// （对齐上游 repo：无参调用打印用法后退出 1，且不额外打印错误信息）。
var errHelpShown = errors.New("help shown")

// globalFlagsWithValue 列出需要独立取值的全局 flag，供 --show-toplevel
// 预扫描时跳过其值，避免把 flag 的值误当成第一个非 flag 词。
var globalFlagsWithValue = map[string]struct{}{
	"--color":                {},
	"--event-log":            {},
	"--git-trace2-event-log": {},
	"--submanifest-path":     {},
}

// globalBoolFlags 列出无取值的全局 bool flag，供 --show-toplevel 预扫描识别；
// 未知 flag 仍交给 cobra 校验，避免预扫描提前退出而掩盖参数错误。
var globalBoolFlags = map[string]struct{}{
	"--paginate": {},
	"--no-pager": {},
	"--trace":    {},
	"--trace-go": {},
	"--time":     {},
}

// mirrorSafeCommands 对齐上游 MirrorSafeCommand 白名单：镜像客户端没有工作树，
// 只允许这些子命令运行（以用户输入的一级子命令名为准）。
// smartsync 继承 sync 的 MirrorSafe 语义；completion/__complete 是 shell 补全
// 所需的工具子命令，也必须放行。repo-go 没有 config 子命令，故不保留该条目。
var mirrorSafeCommands = map[string]struct{}{
	"__complete":       {},
	"__completeNoDesc": {},
	"completion":       {},
	"forall":           {},
	"help":             {},
	"init":             {},
	"list":             {},
	"selfupdate":       {},
	"smartsync":        {},
	"sync":             {},
	"version":          {},
}

func main() {
	// 初始化日志
	log := logger.NewDefaultLogger()
	logFile := os.Getenv("REPO_LOG_FILE")
	if logFile != "" {
		if err := log.SetDebugFile(logFile); err != nil {
			fmt.Printf("warning: failed to set log file %s: %v\n", logFile, err)
		}
	}
	logger.SetGlobalLogger(log)
	// 将全局日志记录器同步注入各 internal 包的包级日志器，
	// 否则 git/config/hook 包内的 Debug/Trace 输出（如 --trace 下的 git 命令轨迹）
	// 始终被默认 Info 级别抑制，--trace/--verbose 对这些包不生效
	git.SetLogger(log)
	git.SetRepositoryLogger(log)
	config.SetLogger(log)
	hook.SetLogger(log)

	// 版本信息（构建时注入）：--version 旗标、version 与 selfupdate 子命令共用
	versionInfo := commands.VersionInfo{Version: version, Commit: commit, Date: date}

	// 在 cobra Find/Args 校验之前处理全局 --show-toplevel：命中时只要求输出
	// 顶层目录，不要求子命令/参数合法（脚本化用法）。
	if handled, err := handleShowToplevel(os.Args[1:], os.Stdout); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "repo: error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	rootCmd := newRootCmd(versionInfo)

	// 执行命令
	if err := rootCmd.Execute(); err != nil {
		switch {
		case errors.Is(err, errHelpShown):
			// 帮助已打印；上游无参调用以非零码退出，且不再输出额外错误。
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "repo: error: %v\n", err)
		os.Exit(1)
	}
}

// newRootCmd 构建根命令（独立函数便于测试无子命令时的 flag 行为）。
func newRootCmd(versionInfo commands.VersionInfo) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:          "repo [--paginate|--no-pager] COMMAND [ARGS]",
		SilenceUsage: true, // 运行时错误不打印 Usage（对齐上游：错误信息单行输出）
		// 错误由 main 统一输出为 "repo: error: ..."，避免 cobra 默认的 "Error:" 前缀
		SilenceErrors: true,
		Short:         "Repo is a tool for managing multiple git repositories",
		Long: `Options:
  -h, --help            show this help message and exit
  --paginate            display command output in the pager
  --no-pager            disable the pager
  --color=COLOR         control color usage: auto, always, never
  --trace               trace git command execution (REPO_TRACE=1)
  --trace-go            trace go command execution
  --time                time repo command execution
  --version             display this version of repo
  --show-toplevel       display the path of the top-level directory of the repo client checkout
  --event-log=EVENT_LOG filename of event log to append timeline to
  --git-trace2-event-log=GIT_TRACE2_EVENT_LOG directory to write git trace2 event log to
  --submanifest-path=REL_PATH submanifest path
        `,
		Version: versionInfo.OneLine(),
		// 无子命令时：--show-toplevel 输出顶层目录；否则显示帮助
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRoot(cmd)
		},
	}

	// --version 旗标输出与 version 子命令同源（默认模板会重复 repo version 前缀）
	rootCmd.SetVersionTemplate("{{.Version}}\n")

	// 设置PersistentPreRunE钩子函数处理全局标志
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// 处理--trace标志
		trace, _ := cmd.Flags().GetBool("trace")
		if trace {
			// 设置REPO_TRACE环境变量
			os.Setenv("REPO_TRACE", "1")
			// 设置日志级别为Trace
			logger.SetLevel(logger.LogLevelTrace)
			logger.Trace("启用了跟踪模式")
		}

		// 处理--trace-go标志
		traceGo, _ := cmd.Flags().GetBool("trace-go")
		if traceGo {
			// 设置Go运行时跟踪
			os.Setenv("GODEBUG", "http2debug=2,gctrace=1")
			logger.Trace("启用了Go运行时跟踪")
		}

		// 处理--color标志（auto/always/never）：非法取值直接报错（对齐上游 argparse choices）
		colorMode, _ := cmd.Flags().GetString("color")
		switch colorMode {
		case "never":
			os.Setenv("NO_COLOR", "1")
		case "always":
			os.Unsetenv("NO_COLOR")
		case "auto", "":
			// auto：非字符设备（管道/文件）时禁用颜色
			if fi, err := os.Stdout.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
				os.Setenv("NO_COLOR", "1")
			}
		default:
			return fmt.Errorf("invalid color option: %s (must be auto, always, or never)", colorMode)
		}

		// MirrorSafe 限制：镜像客户端没有工作树，对齐上游只允许白名单子命令。
		// 判定与报错都使用用户输入的一级子命令名：cobra 的 leaf 名可能是
		// completion 的 bash/__complete，直接用 leaf 名会误拒补全入口。
		// help/version 在一级名上已放行；配置加载失败时交给命令自身报错。
		if cmd != rootCmd {
			name := firstLevelCommandName(cmd)
			if !isMirrorSafeCommand(name) {
				if cfg, err := config.Load(); err == nil {
					if err := mirrorSafeError(name, cfg); err != nil {
						return err
					}
				}
			}
		}

		// 处理--git-trace2-event-log标志
		eventLog, _ := cmd.Flags().GetString("git-trace2-event-log")
		if eventLog != "" {
			// 设置Git Trace2事件日志
			os.Setenv("GIT_TRACE2_EVENT", eventLog)
			logger.Trace("Git Trace2事件日志已设置为: %s", eventLog)
		}

		return nil
	}

	// 全局选项
	rootCmd.PersistentFlags().Bool("paginate", false, "display command output in the pager")
	rootCmd.PersistentFlags().Bool("no-pager", false, "disable the pager")
	rootCmd.PersistentFlags().String("color", "auto", "control color usage: auto, always, never")
	rootCmd.PersistentFlags().Bool("trace", false, "trace git command execution (REPO_TRACE=1)")
	rootCmd.PersistentFlags().Bool("trace-go", false, "trace go command execution")
	rootCmd.PersistentFlags().Bool("time", false, "time repo command execution")
	// --show-toplevel 只作为根命令的本地 flag（对齐上游：全局选项必须位于
	// 子命令之前）；子命令之后的同名 flag 由 cobra 报未知 flag。
	rootCmd.Flags().Bool("show-toplevel", false, "display the path of the top-level directory of the repo client checkout")
	rootCmd.PersistentFlags().String("event-log", "", "filename of event log to append timeline to")
	rootCmd.PersistentFlags().String("git-trace2-event-log", "", "directory to write git trace2 event log to")
	rootCmd.PersistentFlags().String("submanifest-path", "", "submanifest path")

	// 添加子命令
	rootCmd.AddCommand(commands.InitCmd())
	rootCmd.AddCommand(commands.SyncCmd())
	rootCmd.AddCommand(commands.StartCmd())
	rootCmd.AddCommand(commands.StatusCmd())
	rootCmd.AddCommand(commands.DiffCmd())
	rootCmd.AddCommand(commands.UploadCmd())
	rootCmd.AddCommand(commands.ForallCmd())
	rootCmd.AddCommand(commands.ManifestCmd())
	rootCmd.AddCommand(commands.PruneCmd())
	rootCmd.AddCommand(commands.AbandonCmd())
	rootCmd.AddCommand(commands.BranchCmd())
	rootCmd.AddCommand(commands.CheckoutCmd())
	rootCmd.AddCommand(commands.CherryPickCmd())
	rootCmd.AddCommand(commands.DownloadCmd())
	rootCmd.AddCommand(commands.GrepCmd())
	rootCmd.AddCommand(commands.InfoCmd())
	rootCmd.AddCommand(commands.ListCmd())
	rootCmd.AddCommand(commands.RebaseCmd())
	rootCmd.AddCommand(commands.SmartSyncCmd())
	rootCmd.AddCommand(commands.StageCmd())
	rootCmd.AddCommand(commands.DiffManifestsCmd())
	rootCmd.AddCommand(commands.OverviewCmd())
	rootCmd.AddCommand(commands.SelfUpdateCmd(versionInfo))

	// version 子命令：与 --version 旗标同源，另附系统 git 版本（对齐上游 repo version）
	rootCmd.AddCommand(commands.VersionCmd(versionInfo))

	return rootCmd
}

// runRoot 处理无子命令时的根命令行为：
//   - --show-toplevel：输出 repo 顶层目录（可脚本化，退出码 0）；
//   - 其余情况：显示帮助并返回 errHelpShown（退出码 1，对齐上游）。
func runRoot(cmd *cobra.Command) error {
	if showToplevel, _ := cmd.Flags().GetBool("show-toplevel"); showToplevel {
		return printRepoToplevel(cmd.OutOrStdout())
	}
	if err := cmd.Help(); err != nil {
		return fmt.Errorf("failed to show help: %w", err)
	}
	return errHelpShown
}

// showToplevelRequested 判断全局 --show-toplevel 是否出现在第一个非 flag 词之前。
// 子命令之后的同名 flag 不在此处理（交由 cobra/子命令逻辑）。
//
// 预扫描只负责决定是否在 cobra 解析前输出顶层目录：
//   - -h/--help/--version 优先，命中则交给 cobra；
//   - --show-toplevel=value 使用 strconv.ParseBool，非法值交给 cobra 报错；
//   - 未知全局 flag 或非法 --color 值不提前退出，仍由 cobra 校验。
func showToplevelRequested(args []string) bool {
	requested := false
	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			// 选项解析终止：其后参数是位置参数，不再影响已命中的 --show-toplevel。
			return requested
		}
		if arg == "-h" || arg == "--help" || arg == "--version" ||
			strings.HasPrefix(arg, "--help=") || strings.HasPrefix(arg, "--version=") {
			// help/version 优先于 --show-toplevel，交给 cobra 处理。
			return false
		}
		if !strings.HasPrefix(arg, "-") {
			// 第一个非 flag 词（子命令/位置参数）：已命中则仍输出顶层目录，
			// 不要求子命令或参数合法（脚本化用法）。
			return requested
		}

		name, value, hasValue := strings.Cut(arg, "=")
		if name == "--show-toplevel" {
			if !hasValue {
				requested = true
				continue
			}
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				// 非法布尔值交给 cobra 报错，避免提前成功退出掩盖参数错误。
				return false
			}
			requested = enabled
			continue
		}

		if _, ok := globalFlagsWithValue[name]; ok {
			if !hasValue {
				i++ // 跳过该 flag 的独立取值
				if i >= len(args) {
					return false // 缺少取值：交给 cobra 报错
				}
				value = args[i]
			}
			if name == "--color" && !isValidColorMode(value) {
				// 非法 --color 仍由 cobra/PersistentPreRunE 校验。
				return false
			}
			continue
		}

		if _, ok := globalBoolFlags[name]; ok {
			if hasValue {
				if _, err := strconv.ParseBool(value); err != nil {
					return false
				}
			}
			continue
		}

		// 未知全局 flag：交给 cobra 报错，不能提前退出。
		return false
	}
	return requested
}

// isValidColorMode 判断 --color 取值是否合法（与 PersistentPreRunE 保持一致）。
func isValidColorMode(mode string) bool {
	switch mode {
	case "auto", "always", "never":
		return true
	}
	return false
}

// firstLevelCommandName 返回用户输入的一级子命令名（如 repo completion bash
// 返回 completion，repo __complete ... 返回 __complete），而不是 cobra 解析后的
// leaf 名。MirrorSafe 判定必须基于一级子命令，否则 completion 的叶子命令
// 或隐藏的补全命令会被误拒。
func firstLevelCommandName(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	parts := strings.Fields(cmd.CommandPath())
	if len(parts) < 2 {
		return cmd.Name()
	}
	return parts[1]
}

// isMirrorSafeCommand 判断子命令是否在上游 MirrorSafe 白名单内。
func isMirrorSafeCommand(name string) bool {
	_, ok := mirrorSafeCommands[name]
	return ok
}

// mirrorSafeError 返回镜像客户端下不可用子命令的错误；非镜像或白名单命令返回 nil。
func mirrorSafeError(name string, cfg *config.Config) error {
	if cfg == nil || !cfg.Mirror || isMirrorSafeCommand(name) {
		return nil
	}
	return fmt.Errorf("'%s' requires a working directory", name)
}

// handleShowToplevel 在 cobra 解析（Find/Args 校验）之前处理全局 --show-toplevel：
// 命中时即使子命令/参数非法也只需打印 repo 顶层目录。
func handleShowToplevel(args []string, out io.Writer) (bool, error) {
	if !showToplevelRequested(args) {
		return false, nil
	}
	return true, printRepoToplevel(out)
}

// printRepoToplevel 将 repo 顶层目录输出到 w；不在 repo 检出内时返回错误。
func printRepoToplevel(w io.Writer) error {
	top := findRepoTopDir()
	if top == "" {
		return fmt.Errorf("not in a repo client checkout")
	}
	fmt.Fprintln(w, top)
	return nil
}

// findRepoTopDir 从当前目录逐级向上查找包含 .repo 目录的顶层工作区路径，
// 未找到返回空串
func findRepoTopDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if fi, serr := os.Stat(filepath.Join(dir, ".repo")); serr == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
