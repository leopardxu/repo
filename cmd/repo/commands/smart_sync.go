package commands

import (
	"runtime"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/spf13/cobra"
)

// SmartSyncCmd 返回smartsync命令
// smartsync 是 sync -s 的快捷方式，继承 sync 命令的所有选项。
// 注意：flag 定义与 SyncCmd() 保持同步，-f 绑定 --force-broken、
// --manifest-server-password 短 flag 为 -w（与 sync 一致）。
func SmartSyncCmd() *cobra.Command {
	// 创建 sync 命令的选项
	opts := &SyncOptions{
		Jobs:         runtime.NumCPU() * 2,
		RetryFetches: 3,
	}

	cmd := &cobra.Command{
		Use:   "smartsync [<project>...]",
		Short: "Update working tree to the latest known good revision",
		Long:  `The 'repo smartsync' command is a shortcut for sync -s.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 创建日志记录器
			log := logger.NewDefaultLogger()

			// 根据选项设置日志级别
			if opts.Quiet {
				log.SetLevel(logger.LogLevelError)
			} else if opts.Verbose {
				log.SetLevel(logger.LogLevelDebug)
			} else {
				log.SetLevel(logger.LogLevelInfo)
			}

			// 自动启用 SmartSync 模式（这是 smartsync 命令的核心特性）
			opts.SmartSync = true

			// 调用 sync 命令的执行逻辑
			return runSync(opts, args, log, cmd)
		},
	}

	// 添加所有 sync 命令的选项（复用同一份注册逻辑，避免漂移），
	// 除了 --smart-sync（因为已自动启用）
	registerSyncFlags(cmd, opts, false)
	return cmd
}
