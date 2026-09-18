package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Proxy 表示SSH连接代理
type Proxy struct {
	controlMaster bool
	controlPath   string
	sshDir        string
	sshConfig     string
	connections   map[string]*exec.Cmd
	mu            sync.Mutex
}

// NewProxy 创建一个新的SSH代理
func NewProxy() (*Proxy, error) {
	// 获取用户主目录
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("无法获取用户主目录: %w", err)
	}

	// 创建SSH目录
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, fmt.Errorf("无法创建SSH目录: %w", err)
	}

	// 创建控制路径目录
	controlPath := filepath.Join(sshDir, "controlmasters")
	if err := os.MkdirAll(controlPath, 0700); err != nil {
		return nil, fmt.Errorf("无法创建SSH控制路径目录: %w", err)
	}

	// 检查SSH是否支持ControlMaster
	controlMaster := checkControlMasterSupport()

	return &Proxy{
		controlMaster: controlMaster,
		controlPath:   controlPath,
		sshDir:        sshDir,
		sshConfig:     filepath.Join(sshDir, "config"),
		connections:   make(map[string]*exec.Cmd),
	}, nil
}

// Close 关闭SSH代理
func (p *Proxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 关闭所有SSH连接
	for host, cmd := range p.connections {
		if cmd != nil && cmd.Process != nil {
			// 尽力而为：进程可能已自行退出，Kill 失败无需处理
			_ = cmd.Process.Kill()
		}
		delete(p.connections, host)
	}
}

// GetSSHCommand 获取SSH命令
func (p *Proxy) GetSSHCommand(host string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 如果不支持ControlMaster，直接返回普通SSH命令
	if !p.controlMaster {
		return []string{"ssh", host}
	}

	// 检查是否已经有连接
	if _, ok := p.connections[host]; !ok {
		// 创建新连接
		controlPath := filepath.Join(p.controlPath, fmt.Sprintf("%s.sock", host))

		// 启动SSH控制主进程
		cmd := exec.Command("ssh",
			"-o", "ControlMaster=yes",
			"-o", fmt.Sprintf("ControlPath=%s", controlPath),
			"-o", "ControlPersist=yes",
			"-N", host)

		// 非阻塞启动；启动失败时控制套接字不存在，GetSSHCommand 返回的
		// 命令会自动退化为普通 SSH 连接，无需中断
		_ = cmd.Start()

		// 保存连接
		p.connections[host] = cmd
	}

	// 返回使用控制路径的SSH命令
	controlPath := filepath.Join(p.controlPath, fmt.Sprintf("%s.sock", host))
	return []string{"ssh",
		"-o", "ControlMaster=no",
		"-o", fmt.Sprintf("ControlPath=%s", controlPath),
		host}
}

// checkControlMasterSupport 检查SSH是否支持ControlMaster
func checkControlMasterSupport() bool {
	// Windows不支持ControlMaster
	if runtime.GOOS == "windows" {
		return false
	}

	// 检查SSH版本
	cmd := exec.Command("ssh", "-V")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}

	// 解析SSH版本
	versionStr := strings.ToLower(string(output))
	return strings.Contains(versionStr, "openssh")
}
