package commands

import (
	"fmt"
	"strings"

	"github.com/leopardxu/repo-go/internal/git"
	"github.com/spf13/cobra"
)

// VersionInfo 描述构建时通过 -ldflags 注入的版本信息。
// 变量本体在 cmd/repo/main.go（Makefile 注入目标为 main.version 等），
// 此处仅承载传递与格式化；dev/none/unknown 为未注入时的占位值。
type VersionInfo struct {
	Version string
	Commit  string
	Date    string
}

// isUnset 判断字段是否为未注入的占位值
func isUnset(v string) bool {
	return v == "" || v == "none" || v == "unknown"
}

// detailParts 返回版本附注（commit/构建时间），占位值省略
func (v VersionInfo) detailParts() []string {
	parts := make([]string, 0, 2)
	if !isUnset(v.Commit) {
		parts = append(parts, "commit "+v.Commit)
	}
	if !isUnset(v.Date) {
		parts = append(parts, "built "+v.Date)
	}
	return parts
}

// Format 返回 version 子命令的多行输出。
// 对齐上游 subcmds/version.py 的结构（版本行 + 缩进括注行），
// 适配单二进制形态：无 launcher 版本与 manifest 仓库描述。
// 系统 git 版本行由调用方附加（保持本函数纯净可测）。
func (v VersionInfo) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "repo version %s\n", v.Version)
	if parts := v.detailParts(); len(parts) > 0 {
		fmt.Fprintf(&b, "       (%s)\n", strings.Join(parts, ", "))
	}
	return b.String()
}

// OneLine 返回 --version 旗标的单行输出，与 version 子命令同源
func (v VersionInfo) OneLine() string {
	if parts := v.detailParts(); len(parts) > 0 {
		return fmt.Sprintf("repo version %s (%s)", v.Version, strings.Join(parts, ", "))
	}
	return fmt.Sprintf("repo version %s", v.Version)
}

// gitVersion 返回系统 git 版本行（如 "git version 2.43.0"）。
// repo-go 通过 system git 执行实际操作，报告其版本便于诊断；获取失败返回空串。
func gitVersion() string {
	out, err := git.NewRunner().Run("--version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// VersionCmd 返回 version 子命令（对齐上游：显示 repo 版本与系统 git 版本）
func VersionCmd(info VersionInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Display the version of repo",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Print(info.Format())
			if gv := gitVersion(); gv != "" {
				fmt.Println(gv)
			}
		},
	}
}
