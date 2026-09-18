package main

import (
	"fmt"
	"os"
	"path/filepath"

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

	rootCmd := &cobra.Command{
		Use:          "repo [--paginate|--no-pager] COMMAND [ARGS]",
		SilenceUsage: true, // 运行时错误不打印 Usage（对齐上游：错误信息单行输出）
		Short:        "Repo is a tool for managing multiple git repositories",
		Long: `Usage: repo [--paginate|--no-pager] COMMAND [ARGS]

Options:
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
	}

	// --version 旗标输出与 version 子命令同源（默认模板会重复 repo version 前缀）
	rootCmd.SetVersionTemplate("{{.Version}}\n")

	// 设置PersistentPreRun钩子函数处理全局标志
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		// 处理--trace标志
		trace, _ := cmd.Flags().GetBool("trace")
		if trace {
			// 设置REPO_TRACE环境变量
			os.Setenv("REPO_TRACE", "1")
			// 设置日志级别为Trace
			logger.SetLevel(logger.LogLevelTrace)
			log.Trace("启用了跟踪模式")
		}

		// 处理--trace-go标志
		traceGo, _ := cmd.Flags().GetBool("trace-go")
		if traceGo {
			// 设置Go运行时跟踪
			os.Setenv("GODEBUG", "http2debug=2,gctrace=1")
			log.Trace("启用了Go运行时跟踪")
		}

		// 处理--color标志（auto/always/never）：非法取值直接报错退出（对齐上游 argparse choices）
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
			fmt.Fprintf(os.Stderr, "repo: error: invalid color option: %s (must be auto, always, or never)\n", colorMode)
			os.Exit(1)
		}

		// 处理--show-toplevel标志：打印repo工作区顶层目录路径后退出（对齐上游）
		if showToplevel, _ := cmd.Flags().GetBool("show-toplevel"); showToplevel {
			top := findRepoTopDir()
			if top == "" {
				fmt.Fprintln(os.Stderr, "repo: error: not in a repo client checkout")
				os.Exit(1)
			}
			fmt.Println(top)
			os.Exit(0)
		}

		// 处理--git-trace2-event-log标志
		eventLog, _ := cmd.Flags().GetString("git-trace2-event-log")
		if eventLog != "" {
			// 设置Git Trace2事件日志
			os.Setenv("GIT_TRACE2_EVENT", eventLog)
			log.Trace("Git Trace2事件日志已设置为: %s", eventLog)
		}
	}

	// 全局选项
	rootCmd.PersistentFlags().Bool("paginate", false, "display command output in the pager")
	rootCmd.PersistentFlags().Bool("no-pager", false, "disable the pager")
	rootCmd.PersistentFlags().String("color", "auto", "control color usage: auto, always, never")
	rootCmd.PersistentFlags().Bool("trace", false, "trace git command execution (REPO_TRACE=1)")
	rootCmd.PersistentFlags().Bool("trace-go", false, "trace go command execution")
	rootCmd.PersistentFlags().Bool("time", false, "time repo command execution")
	rootCmd.PersistentFlags().Bool("show-toplevel", false, "display the path of the top-level directory of the repo client checkout")
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

	// 执行命令
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
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
