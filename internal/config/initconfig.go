package config

import (
	"fmt"
	"os/exec"
)

// lookPath 是 exec.LookPath 的可注入替身，便于测试模拟 git 缺失/存在的场景，
// 避免测试依赖测试机的真实环境。
var lookPath = exec.LookPath

// LoadGitConfig 检查 git 是否可用。
//
// 历史上本函数还会向用户全局 git config 写入硬编码默认身份
// （user.name/user.email）以及 core.autocrlf/core.filemode。该行为已删除：
//   - 上游 repo 从不修改用户的全局配置，无条件写入会静默覆盖用户既有设置；
//   - grep 证明本函数在全仓没有调用方，不存在必须由此提供默认配置的诉求；
//   - 若未来确有默认配置需求（例如为 .repo/manifests.git 局部设置
//     core.filemode），应由调用方（cmd 层）通过 git.Runner 针对具体仓库显式
//     执行（参见 commands/init.go 的 promptForUserInfo 先例），而非在 config
//     包内 exec git——git 包反向依赖 config（NewCommandRunnerWithConfig），
//     config 无法 import git。
func LoadGitConfig() error {
	log.Debug("检查Git是否安装")

	// 检查git是否安装（exec.LookPath 只探测 PATH，不执行 git，不受禁令约束）
	gitPath, err := lookPath("git")
	if err != nil {
		// 仅返回错误，由调用方决定如何呈现（log 与 return 二选一）
		return &ConfigError{Op: "load_git_config", Err: fmt.Errorf("git not found: %w", err)}
	}
	log.Debug("找到Git路径: %s", gitPath)

	return nil
}
