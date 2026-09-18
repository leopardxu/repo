package config

import (
	"errors"
	"os/exec"
	"testing"
)

// TestLoadGitConfig 验证 LoadGitConfig 的 git 可用性检查：
// 通过注入 lookPath 替身模拟 git 存在/缺失，不依赖测试机真实环境，
// 更不会执行任何 git config 命令（本测试如果误触全局配置即为实现回归）。
func TestLoadGitConfig(t *testing.T) {
	tests := []struct {
		name        string
		lookPath    func(string) (string, error)
		wantErr     bool
		errOp       string // 期望的 ConfigError.Op，wantErr 为 true 时校验
		wantWrapped error  // 期望被包装的哨兵错误，可为 nil
	}{
		{
			name: "git已安装时返回nil",
			lookPath: func(string) (string, error) {
				return "/usr/bin/git", nil
			},
			wantErr: false,
		},
		{
			name: "git缺失时返回ConfigError",
			lookPath: func(string) (string, error) {
				return "", exec.ErrNotFound
			},
			wantErr:     true,
			errOp:       "load_git_config",
			wantWrapped: exec.ErrNotFound,
		},
		{
			name: "LookPath其他错误同样包装为ConfigError",
			lookPath: func(string) (string, error) {
				return "", errors.New("permission denied")
			},
			wantErr: true,
			errOp:   "load_git_config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := lookPath
			lookPath = tt.lookPath
			t.Cleanup(func() { lookPath = orig })

			err := LoadGitConfig()

			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望返回错误，实际为 nil")
				}
				var cfgErr *ConfigError
				if !errors.As(err, &cfgErr) {
					t.Fatalf("期望错误为 *ConfigError，实际为 %T: %v", err, err)
				}
				if cfgErr.Op != tt.errOp {
					t.Errorf("ConfigError.Op = %q, 期望 %q", cfgErr.Op, tt.errOp)
				}
				if tt.wantWrapped != nil && !errors.Is(err, tt.wantWrapped) {
					t.Errorf("错误链中应包含 %v，实际: %v", tt.wantWrapped, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，实际得到错误: %v", err)
			}
		})
	}
}
