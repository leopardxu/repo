package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/leopardxu/repo-go/internal/logger"
)

// 包级别的日志记录器
var log logger.Logger = logger.NewDefaultLogger()
var logMu sync.RWMutex

// SetLogger 设置包级别的日志记录器
func SetLogger(l logger.Logger) {
	if l != nil {
		logMu.Lock()
		defer logMu.Unlock()
		log = l
	}
}

// HookError 表示hook操作过程中的错误
type HookError struct { //nolint:revive // 命名遵循 CLAUDE.md 约定的 XxxError 错误类型形态，保持包内 API 稳定
	Op   string // 操作名称
	Path string // 文件路径
	Err  error  // 原始错误
}

func (e *HookError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("hook error: %s failed for '%s': %v", e.Op, e.Path, e.Err)
	}
	return fmt.Sprintf("hook error: %s failed: %v", e.Op, e.Err)
}

func (e *HookError) Unwrap() error {
	return e.Err
}

// 文件操作的重试配
const (
	maxRetries = 3
	retryDelay = 100 * time.Millisecond
)

// 预定义的hook模板
var hookTemplates = map[string]string{
	"pre-commit": `#!/bin/bash
# try to find full path of this APP
set -o pipefail
__APP_FULLPATH="${BASH_SOURCE[0]}";
if [[ x"$__APP_FULLPATH" =~ / ]]; then
    # / is found in path, using readlink to expend to abslute path
    APP_FULLPATH=$(readlink -f "$__APP_FULLPATH");
else
    # / not found in path, try to find with which command
    APP_FULLPATH=$(which "$__APP_FULLPATH");
    # not in $PATH, try to find in current directory
    if [[ -z "$APP_FULLPATH" ]]; then
        APP_FULLPATH="$(pwd)/${__APP_FULLPATH}";
    fi
fi
if [[ ! -e "${APP_FULLPATH}" ]]; then
    echo "Cannot determine location of this script";
    exit 1;
fi

APP_DIR=$(dirname "${APP_FULLPATH}")
APP_NAME=$(basename "${APP_FULLPATH}")

# 从 hook 所在位置向上查找包含 .repo 的 repo 顶层目录。
# 优先从 git 工作树（hook 由 git 以工作树根为 cwd 调用）开始；
# 工作树可能位于 repo 顶层之外，此时回退到 hook 文件自身所在目录。
# hook 既可能是 .repo/hooks 下的符号链接，也可能被复制到
# <项目>/.git/hooks 或镜像布局的 <项目>.git/hooks。
find_repo_top() {
    local dir="$1"
    while [[ -n "${dir}" ]]; do
        if [[ -d "${dir}/.repo" ]]; then
            printf '%s' "${dir}"
            return 0
        fi
        if [[ "${dir}" == "/" || "${dir}" == "." ]]; then
            break
        fi
        dir=$(dirname "${dir}")
    done
    return 1
}

WORKTREE=$(git rev-parse --show-toplevel 2>/dev/null)
REPO_TOP=""
if [[ -n "${WORKTREE}" ]]; then
    REPO_TOP=$(find_repo_top "${WORKTREE}")
fi
if [[ -z "${REPO_TOP}" ]]; then
    REPO_TOP=$(find_repo_top "${APP_DIR}")
fi

if [[ -z "${REPO_TOP}" ]]; then
    echo "No .repo directory found, ignore pre-commit check" >&2;
    exit 0;
fi

HOOK_DIR=$(readlink -f "${REPO_TOP}/.repo/cixtools/hooks")
if [[ ! -e "${HOOK_DIR}" ]]; then
    echo "No hook found, ignore pre-commit check" >&2;
    exit 0;
fi

# WORKTREE 指向 repo 顶层，而不是当前项目工作树（hook 脚本的参数约定）。
WORKTREE="${REPO_TOP}"
GIT_DIR=$(git rev-parse --git-dir 2>/dev/null)
if [[ -z "${GIT_DIR}" ]]; then
    echo "Error: not a git repository, quit $GIT_DIR" >&2;
    exit 1;
fi

if [[ ! -e "${WORKTREE}/.cix-ciconfig" ]]; then
    exit 0;  # no ci config found, skip silently
fi

set -e
# 先把配置读入数组，再在数组上循环：绝不能把循环放在 sed 管道的右侧，
# 否则 hook 脚本读取 stdin 时会吞掉后续配置条目。这里用 while read + 进程替换
# （macOS 自带的 bash 3.2 不支持一次性读入数组的内建命令）：循环不在管道中，
# 配置全部读完之后才执行 hook，hook 继承的是原始 stdin。
# while 条件里的 || [[ -n ... ]] 保证末行无换行符时也不丢条目。
hook_scripts=()
while IFS= read -r script_name || [[ -n "${script_name}" ]]; do
    hook_scripts+=("${script_name}")
done < <(sed -n 's#^pre-commit[[:space:]]*=[[:space:]]*##p' "${WORKTREE}/.cix-ciconfig")
for script_name in "${hook_scripts[@]}"; do
    # 去掉行尾 CR（CRLF 配置文件）后再裁掉行尾空白/Tab（保留内部空格），
    # 空值直接跳过，避免执行空路径。
    script_name="${script_name%$'\r'}"
    script_name="${script_name%"${script_name##*[![:space:]]}"}"
    [[ -z "${script_name}" ]] && continue

    # 绝对路径直接执行；相对路径拼到 hooks/tools/ 下。
    if [[ "${script_name}" == /* ]]; then
        script_path="${script_name}"
    else
        script_path="${HOOK_DIR}/${script_name}"
    fi

    # 用数组传参执行：脚本名含空格时仍是单个参数，无需二次解析。
    hook_cmd=("${script_path}" "${WORKTREE}" "${GIT_DIR}")
    echo "\"${hook_cmd[0]}\" \"${WORKTREE}\" \"${GIT_DIR}\"" >&2
    rc=0
    "${hook_cmd[@]}" || rc=$?
    if [[ $rc -ne 0 ]]; then
        exit $rc;  # quit if any hook failed
    fi
done
`,
	"commit-msg": `#!/bin/sh
# From Gerrit Code Review 3.1.3
#
# Part of Gerrit Code Review (https://www.gerritcodereview.com/)
#
# Copyright (C) 2009 The Android Open Source Project
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# avoid [[ which is not POSIX sh.
if test "$#" != 1 ; then
  echo "$0 requires an argument."
  exit 1
fi

if test ! -f "$1" ; then
  echo "file does not exist: $1"
  exit 1
fi

# Do not create a change id if requested
if test "false" = "$(git config --bool --get gerrit.createChangeId)" ; then
  exit 0
fi

# $RANDOM will be undefined if not using bash, so don't use set -u
random=$( (whoami ; hostname ; date; cat $1 ; echo $RANDOM) | git hash-object --stdin)
dest="$1.tmp.${random}"

trap 'rm -f "${dest}"' EXIT

if ! git stripspace --strip-comments < "$1" > "${dest}" ; then
   echo "cannot strip comments from $1"
   exit 1
fi

if test ! -s "${dest}" ; then
  echo "file is empty: $1"
  exit 1
fi

# Avoid the --in-place option which only appeared in Git 2.8
# Avoid the --if-exists option which only appeared in Git 2.15
if ! git -c trailer.ifexists=doNothing interpret-trailers \
      --trailer "Change-Id: I${random}" < "$1" > "${dest}" ; then
  echo "cannot insert change-id line in $1"
  exit 1
fi

if ! mv "${dest}" "$1" ; then
  echo "cannot mv ${dest} to $1"
  exit 1
fi
`,
	"pre-auto-gc": `#!/bin/sh
#
# An example hook script to verify if you are on battery, in case you
# are running Windows, Linux or OS X. Called by git-gc --auto with no
# arguments. The hook should exit with non-zero status after issuing an
# appropriate message if it wants to stop the auto repacking.

# This program is free software; you can redistribute it and/or modify
# it under the terms of the GNU General Public License as published by
# the Free Software Foundation; either version 2 of the License, or
# (at your option) any later version.
#
# This program is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
# GNU General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program; if not, write to the Free Software
# Foundation, Inc., 59 Temple Place, Suite 330, Boston, MA  02111-1307  USA

if uname -s | grep -q "_NT-"
then
        if test -x $SYSTEMROOT/System32/Wbem/wmic
        then
                STATUS=$(wmic path win32_battery get batterystatus /format:list | tr -d '\r\n')
                [ "$STATUS" = "BatteryStatus=2" ] && exit 0 || exit 1
        fi
        exit 0
fi

if test -x /sbin/on_ac_power && (/sbin/on_ac_power;test $? -ne 1)
then
        exit 0
elif test "$(cat /sys/class/power_supply/AC/online 2>/dev/null)" = 1
then
        exit 0
elif grep -q 'on-line' /proc/acpi/ac_adapter/AC/state 2>/dev/null
then
        exit 0
elif grep -q '0x01$' /proc/apm 2>/dev/null
then
        exit 0
elif grep -q "AC Power \+: 1" /proc/pmu/info 2>/dev/null
then
        exit 0
elif test -x /usr/bin/pmset && /usr/bin/pmset -g batt |
        grep -q "drawing from 'AC Power'"
then
        exit 0
elif test -d /sys/bus/acpi/drivers/battery && test 0 = \
  "$(find /sys/bus/acpi/drivers/battery/ -type l | wc -l)";
then
        # No battery exists.
        exit 0
fi

echo "Auto packing deferred; not on AC"
exit 1
`,
}

// InitHooks 初始化Git hooks
func InitHooks(repoDir string) error {
	log.Debug("初始化Git hooks: %s", repoDir)

	// 创建hooks目录
	hooksDir := filepath.Join(repoDir, ".repo", "hooks")
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		log.Error("创建hooks目录failed: %v", err)
		return &HookError{
			Op:   "init_hooks",
			Path: hooksDir,
			Err:  err,
		}
	}

	// 使用并发创建hook文件
	var wg sync.WaitGroup
	errorCh := make(chan error, len(hookTemplates))

	for hookName, hookContent := range hookTemplates {
		wg.Add(1)
		go func(name, content string) {
			defer wg.Done()

			hookPath := filepath.Join(hooksDir, name)

			// 检查文件是否已存在且内容相同
			if fileExists(hookPath) {
				existingContent, err := os.ReadFile(hookPath)
				if err == nil && string(existingContent) == content {
					// 内容一致无需重写，但仍需保证已有 hook 具有执行权限
					if chmodErr := os.Chmod(hookPath, 0755); chmodErr != nil {
						errorCh <- &HookError{
							Op:   "chmod_hook",
							Path: hookPath,
							Err:  chmodErr,
						}
						return
					}
					log.Debug("Hook文件已存在且内容相同，跳过创建 %s", hookPath)
					return
				}
			}

			// 使用重试机制写入文件
			var err error
			for i := 0; i < maxRetries; i++ {
				err = os.WriteFile(hookPath, []byte(content), 0755)
				if err == nil {
					log.Debug("成功创建hook文件: %s", hookPath)
					break
				}

				log.Debug("创建hook文件failed，尝试重(%d/%d): %v", i+1, maxRetries, err)
				time.Sleep(retryDelay)
			}

			if err != nil {
				errorCh <- &HookError{
					Op:   "create_hook",
					Path: hookPath,
					Err:  err,
				}
				return
			}

			// 已存在文件的权限不受 os.WriteFile 的 perm 参数影响，
			// 因此写入后显式设置 0755，确保 hook 可执行。
			if err := os.Chmod(hookPath, 0755); err != nil {
				errorCh <- &HookError{
					Op:   "chmod_hook",
					Path: hookPath,
					Err:  err,
				}
			}
		}(hookName, hookContent)
	}

	// 等待所有goroutine完成
	wg.Wait()
	close(errorCh)

	// 收集全部错误，避免只返回其中一个而掩盖其他失败
	errs := make([]error, 0, cap(errorCh))
	for err := range errorCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		log.Error("初始化Git hooks failed: %d 个错误", len(errs))
		return errors.Join(errs...)
	}

	log.Info("成功初始化所有Git hooks")
	return nil
}

// CreateRepoGitConfig 创建repo.git配置文件
func CreateRepoGitConfig(repoDir string) error {
	log.Debug("创建repo.git配置文件: %s", repoDir)

	// 创建.repo/repo.git文件
	configPath := filepath.Join(repoDir, ".repo", "repo.git")

	// 检查文件是否已存在
	if fileExists(configPath) {
		log.Debug("repo.git配置文件已存 %s", configPath)
		return nil
	}

	content := `[core]
	repositoryformatversion = 0
	filemode = true
	bare = true
[filter "lfs"]
	clean = git-lfs clean -- %f
	smudge = git-lfs smudge -- %f
	process = git-lfs filter-process
	required = true
`

	// 确保目录存在
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		log.Error("创建配置目录failed: %v", err)
		return &HookError{
			Op:   "create_config_dir",
			Path: configDir,
			Err:  err,
		}
	}

	// 使用重试机制写入文件
	var err error
	for i := 0; i < maxRetries; i++ {
		err = os.WriteFile(configPath, []byte(content), 0644)
		if err == nil {
			log.Info("成功创建repo.git配置文件: %s", configPath)
			break
		}

		log.Debug("创建repo.git配置文件failed，尝试重(%d/%d): %v", i+1, maxRetries, err)
		time.Sleep(retryDelay)
	}

	if err != nil {
		return &HookError{
			Op:   "create_repo_git_config",
			Path: configPath,
			Err:  err,
		}
	}

	return nil
}

// CreateRepoGitconfig 创建repo.gitconfig文件
func CreateRepoGitconfig(repoDir string) error {
	log.Debug("创建repo.gitconfig文件: %s", repoDir)

	// 创建.repo/repo.gitconfig文件
	configPath := filepath.Join(repoDir, ".repo", "repo.gitconfig")

	// 检查文件是否已存在
	if fileExists(configPath) {
		log.Debug("repo.gitconfig文件已存 %s", configPath)
		return nil
	}

	content := `[filter "lfs"]
	clean = git-lfs clean -- %f
	smudge = git-lfs smudge -- %f
	process = git-lfs filter-process
	required = true
`

	// 确保目录存在
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		log.Error("创建配置目录failed: %v", err)
		return &HookError{
			Op:   "create_config_dir",
			Path: configDir,
			Err:  err,
		}
	}

	// 使用重试机制写入文件
	var err error
	for i := 0; i < maxRetries; i++ {
		err = os.WriteFile(configPath, []byte(content), 0644)
		if err == nil {
			log.Info("成功创建repo.gitconfig文件: %s", configPath)
			break
		}

		log.Debug("创建repo.gitconfig文件failed，尝试重(%d/%d): %v", i+1, maxRetries, err)
		time.Sleep(retryDelay)
	}

	if err != nil {
		return &HookError{
			Op:   "create_repo_gitconfig",
			Path: configPath,
			Err:  err,
		}
	}

	return nil
}

// LinkHooks 将hooks链接到项目目
func LinkHooks(projectDir string, hooksDir string) error {
	log.Debug("链接hooks到项目目 %s -> %s", hooksDir, projectDir)

	// 检查项目目录是否存
	if !fileExists(projectDir) {
		log.Error("项目目录不存 %s", projectDir)
		return &HookError{
			Op:   "link_hooks",
			Path: projectDir,
			Err:  fmt.Errorf("project directory does not exist"),
		}
	}

	// 检查hooks目录是否存在
	if !fileExists(hooksDir) {
		log.Error("hooks目录不存 %s", hooksDir)
		return &HookError{
			Op:   "link_hooks",
			Path: hooksDir,
			Err:  fmt.Errorf("hooks directory does not exist"),
		}
	}

	// 确保项目git/hooks目录存在
	projectHooksDir := filepath.Join(projectDir, ".git", "hooks")
	if err := os.MkdirAll(projectHooksDir, 0755); err != nil {
		log.Error("创建项目hooks目录failed: %v", err)
		return &HookError{
			Op:   "create_project_hooks_dir",
			Path: projectHooksDir,
			Err:  err,
		}
	}

	// 遍历hooks目录中的所有文
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		log.Error("读取hooks目录failed: %v", err)
		return &HookError{
			Op:   "read_hooks_dir",
			Path: hooksDir,
			Err:  err,
		}
	}

	// 使用并发处理hook文件
	var wg sync.WaitGroup
	errorCh := make(chan error, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		wg.Add(1)
		go func(e os.DirEntry) {
			defer wg.Done()

			// 源文件和目标文件路径
			srcPath := filepath.Join(hooksDir, e.Name())
			dstPath := filepath.Join(projectHooksDir, e.Name())

			// 尝试使用符号链接（在支持的系统上）
			if trySymlink(srcPath, dstPath) {
				log.Debug("成功创建符号链接: %s -> %s", dstPath, srcPath)
				return
			}

			// 如果目标文件已存在，先删
			if fileExists(dstPath) {
				if err := os.Remove(dstPath); err != nil {
					errorCh <- &HookError{
						Op:   "remove_existing_hook",
						Path: dstPath,
						Err:  err,
					}
					return
				}
			}

			// 读取源hook文件内容
			srcContent, err := os.ReadFile(srcPath)
			if err != nil {
				errorCh <- &HookError{
					Op:   "read_hook_file",
					Path: srcPath,
					Err:  err,
				}
				return
			}

			// 使用重试机制写入文件
			for i := 0; i < maxRetries; i++ {
				err = os.WriteFile(dstPath, srcContent, 0755)
				if err == nil {
					log.Debug("成功复制hook文件: %s -> %s", srcPath, dstPath)
					break
				}

				log.Debug("复制hook文件failed，尝试重(%d/%d): %v", i+1, maxRetries, err)
				time.Sleep(retryDelay)
			}

			if err != nil {
				errorCh <- &HookError{
					Op:   "write_hook_file",
					Path: dstPath,
					Err:  err,
				}
			}
		}(entry)
	}

	// 等待所有goroutine完成
	wg.Wait()
	close(errorCh)

	// 收集全部错误，避免只返回其中一个而掩盖其他失败
	errs := make([]error, 0, cap(errorCh))
	for err := range errorCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		log.Error("链接hooks到项目目录 failed: %d 个错误", len(errs))
		return errors.Join(errs...)
	}

	log.Info("成功链接所有hooks到项目目 %s", projectDir)
	return nil
}

// fileExists 检查文件或目录是否存在
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// trySymlink 尝试创建符号链接；当前平台/文件系统不支持时返回 false，
// 且不会破坏已存在的目标文件（由调用方回退到复制）。
func trySymlink(src, dst string) bool {
	dir := filepath.Dir(dst)

	// 先在目标目录创建临时符号链接，确认具备符号链接能力后再替换目标。
	tmpFile, err := os.CreateTemp(dir, ".repo-symlink-*")
	if err != nil {
		return false
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	// 释放临时文件名，供 os.Symlink 使用
	_ = os.Remove(tmpPath)

	if err := os.Symlink(src, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return false
	}

	// rename 可原子替换目标（Unix）；若平台不允许覆盖已有文件（如 Windows），
	// 在符号链接已就绪的前提下删除旧目标后重试。
	if err := os.Rename(tmpPath, dst); err != nil {
		if removeErr := os.Remove(dst); removeErr != nil && !os.IsNotExist(removeErr) {
			_ = os.Remove(tmpPath)
			return false
		}
		if err := os.Rename(tmpPath, dst); err != nil {
			_ = os.Remove(tmpPath)
			return false
		}
	}

	return true
}
