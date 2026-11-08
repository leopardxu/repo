package repo_sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	logpkg "github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/manifest"
	"github.com/leopardxu/repo-go/internal/project"
)

// mustWriteFile 测试辅助：创建文件并写入内容
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("创建目录 %s failed: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写入文件 %s failed: %v", path, err)
	}
}

// mustSymlink 测试辅助：创建符号链接，不支持时跳过测试
func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("当前环境不支持符号链接，跳过测试: %v", err)
	}
}

// resolveLinkTarget 解析链接实际指向的绝对路径
func resolveLinkTarget(t *testing.T, linkPath string) string {
	t.Helper()
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("读取符号链接 %s failed: %v", linkPath, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(linkPath), target)
	}
	return filepath.Clean(target)
}

func TestSafeExpandPath(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()

	mustWriteFile(t, filepath.Join(base, "dir", "file.txt"), "hello")
	mustWriteFile(t, filepath.Join(base, "dir", "sub", "deep.txt"), "deep")
	mustWriteFile(t, filepath.Join(base, "final.txt"), "final")
	mustSymlink(t, outside, filepath.Join(base, "linkdir"))

	tests := []struct {
		name      string
		subpath   string
		skipFinal bool
		want      string // 期望的绝对路径，wantErr 为空时校验
		wantErr   string // 期望错误信息包含的子串
	}{
		{name: "正常相对路径", subpath: "dir/file.txt", want: filepath.Join(base, "dir", "file.txt")},
		{name: "不存在的中间目录", subpath: "missing/sub/file.txt", want: filepath.Join(base, "missing", "sub", "file.txt")},
		{name: "重复分隔符", subpath: "dir//file.txt", want: filepath.Join(base, "dir", "file.txt")},
		{name: "逃逸父目录", subpath: "../../x", wantErr: ".."},
		{name: "内嵌父目录", subpath: "dir/../x", wantErr: ".."},
		{name: "点目录组件", subpath: "./file", wantErr: "."},
		{name: "绝对路径按相对处理", subpath: "/etc/passwd", want: filepath.Join(base, "etc", "passwd")},
		{name: "绝对路径含父目录", subpath: "/etc/../../x", wantErr: ".."},
		{name: "中间符号链接", subpath: "linkdir/evil.txt", wantErr: "符号链接"},
		{name: "最终符号链接", subpath: "linkdir", wantErr: "符号链接"},
		{name: "skipFinal 合法", subpath: "dir/newlink", skipFinal: true, want: filepath.Join(base, "dir", "newlink")},
		{name: "skipFinal 绝对路径", subpath: "/abs/link", skipFinal: true, want: filepath.Join(base, "abs", "link")},
		{name: "skipFinal 现有普通文件", subpath: "final.txt", skipFinal: true, want: filepath.Join(base, "final.txt")},
		{name: "skipFinal 现有符号链接目标", subpath: "linkdir", skipFinal: true, want: filepath.Join(base, "linkdir")},
		{name: "skipFinal 中间符号链接", subpath: "linkdir/link", skipFinal: true, wantErr: "符号链接"},
		{name: "skipFinal 拒绝父目录", subpath: "..", skipFinal: true, wantErr: ".."},
		{name: "skipFinal 拒绝父目录结尾", subpath: "dir/..", skipFinal: true, wantErr: ".."},
		{name: "skipFinal 拒绝点目录", subpath: ".", skipFinal: true, wantErr: "."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := safeExpandPath(base, tt.subpath, tt.skipFinal)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("safeExpandPath(%q) 期望返回错误，实际得到 %q", tt.subpath, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tt.wantErr)
				}
				if got != "" {
					t.Fatalf("出错时不应返回路径，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("safeExpandPath(%q) 返回意外错误: %v", tt.subpath, err)
			}
			if got != tt.want {
				t.Fatalf("safeExpandPath(%q) = %q, want %q", tt.subpath, got, tt.want)
			}
			// 结果必须始终位于 base 之内
			rel, err := filepath.Rel(base, got)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("结果 %q 逃逸出 base %q (rel=%q, err=%v)", got, base, rel, err)
			}
		})
	}
}

func TestProcessLinkAndCopyFiles(t *testing.T) {
	const projectName = "test-project"

	repoRoot := t.TempDir()
	outside := t.TempDir()

	worktree := filepath.Join(repoRoot, "proj")
	mustWriteFile(t, filepath.Join(worktree, "src.txt"), "content")
	mustWriteFile(t, filepath.Join(worktree, "sub", "other.txt"), "other")
	mustWriteFile(t, filepath.Join(outside, "target.txt"), "original")

	// 指向 repoRoot 之外的目录/文件，用于构造越界与写穿场景
	mustSymlink(t, outside, filepath.Join(repoRoot, "evil"))
	mustSymlink(t, filepath.Join(outside, "target.txt"), filepath.Join(repoRoot, "destlink"))

	newEngine := func() *Engine {
		l := logpkg.NewDefaultLogger()
		l.SetLevel(logpkg.LogLevelError)
		return &Engine{repoRoot: repoRoot, logger: l}
	}
	newProject := func(cp []project.CopyFile, ln []project.LinkFile) *project.Project {
		return &project.Project{
			Name:      projectName,
			Worktree:  "proj",
			Copyfiles: cp,
			Linkfiles: ln,
		}
	}

	t.Run("copyfile 合法相对路径", func(t *testing.T) {
		eng := newEngine()
		p := newProject([]project.CopyFile{{Src: "src.txt", Dest: "copied/out.txt"}}, nil)
		if err := eng.processLinkAndCopyFiles(p); err != nil {
			t.Fatalf("处理合法 copyfile 返回错误: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(repoRoot, "copied", "out.txt"))
		if err != nil {
			t.Fatalf("读取复制结果 failed: %v", err)
		}
		if string(data) != "content" {
			t.Fatalf("复制内容 = %q, want %q", string(data), "content")
		}
	})

	t.Run("copyfile 绝对目标限制在 repoRoot", func(t *testing.T) {
		eng := newEngine()
		p := newProject([]project.CopyFile{{Src: "src.txt", Dest: "/abs/out.txt"}}, nil)
		if err := eng.processLinkAndCopyFiles(p); err != nil {
			t.Fatalf("处理绝对路径 copyfile 返回错误: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(repoRoot, "abs", "out.txt"))
		if err != nil {
			t.Fatalf("读取复制结果 failed: %v", err)
		}
		if string(data) != "content" {
			t.Fatalf("复制内容 = %q, want %q", string(data), "content")
		}
	})

	t.Run("copyfile 越界目标", func(t *testing.T) {
		eng := newEngine()
		p := newProject([]project.CopyFile{{Src: "src.txt", Dest: "../../pwn-copy.txt"}}, nil)
		err := eng.processLinkAndCopyFiles(p)
		if err == nil {
			t.Fatal("越界 copyfile 目标应返回错误")
		}
		if !strings.Contains(err.Error(), projectName) {
			t.Fatalf("错误信息应包含项目名 %q，实际: %v", projectName, err)
		}
		escaped := filepath.Clean(filepath.Join(repoRoot, "../../pwn-copy.txt"))
		if _, statErr := os.Lstat(escaped); !os.IsNotExist(statErr) {
			t.Fatalf("越界目标 %s 不应被创建 (err=%v)", escaped, statErr)
		}
	})

	t.Run("copyfile 越界源", func(t *testing.T) {
		eng := newEngine()
		p := newProject([]project.CopyFile{{Src: "../../secret.txt", Dest: "stolen.txt"}}, nil)
		err := eng.processLinkAndCopyFiles(p)
		if err == nil {
			t.Fatal("含 .. 的 copyfile 源应返回错误")
		}
		if !strings.Contains(err.Error(), projectName) {
			t.Fatalf("错误信息应包含项目名 %q，实际: %v", projectName, err)
		}
		if _, statErr := os.Lstat(filepath.Join(repoRoot, "stolen.txt")); !os.IsNotExist(statErr) {
			t.Fatalf("越界源不应产生目标文件 (err=%v)", statErr)
		}
	})

	t.Run("copyfile 中间符号链接", func(t *testing.T) {
		eng := newEngine()
		p := newProject([]project.CopyFile{{Src: "src.txt", Dest: "evil/pwn.txt"}}, nil)
		if err := eng.processLinkAndCopyFiles(p); err == nil {
			t.Fatal("经过符号链接的 copyfile 目标应返回错误")
		}
		if _, statErr := os.Lstat(filepath.Join(outside, "pwn.txt")); !os.IsNotExist(statErr) {
			t.Fatalf("不应通过符号链接写出 repoRoot (err=%v)", statErr)
		}
	})

	t.Run("copyfile 拒绝已存在的符号链接目标", func(t *testing.T) {
		eng := newEngine()
		p := newProject([]project.CopyFile{{Src: "src.txt", Dest: "destlink"}}, nil)
		if err := eng.processLinkAndCopyFiles(p); err == nil {
			t.Fatal("copyfile 目标是符号链接时应返回错误")
		}
		data, err := os.ReadFile(filepath.Join(outside, "target.txt"))
		if err != nil {
			t.Fatalf("读取链接目标 failed: %v", err)
		}
		if string(data) != "original" {
			t.Fatalf("符号链接目标被写穿，内容 = %q", string(data))
		}
	})

	t.Run("copyfile src 为当前目录", func(t *testing.T) {
		eng := newEngine()
		p := newProject([]project.CopyFile{{Src: ".", Dest: "dot-copy.txt"}}, nil)
		err := eng.processLinkAndCopyFiles(p)
		if err == nil {
			t.Fatal("copyfile src=. 指向目录，应返回错误")
		}
		if !strings.Contains(err.Error(), projectName) {
			t.Fatalf("错误信息应包含项目名 %q，实际: %v", projectName, err)
		}
	})

	t.Run("linkfile 合法相对目标", func(t *testing.T) {
		eng := newEngine()
		p := newProject(nil, []project.LinkFile{{Src: "src.txt", Dest: "links/link.txt"}})
		if err := eng.processLinkAndCopyFiles(p); err != nil {
			t.Fatalf("处理合法 linkfile 返回错误: %v", err)
		}
		linkPath := filepath.Join(repoRoot, "links", "link.txt")
		if got := resolveLinkTarget(t, linkPath); got != filepath.Join(worktree, "src.txt") {
			t.Fatalf("链接目标 = %q, want %q", got, filepath.Join(worktree, "src.txt"))
		}
		data, err := os.ReadFile(linkPath)
		if err != nil {
			t.Fatalf("通过链接读取文件 failed: %v", err)
		}
		if string(data) != "content" {
			t.Fatalf("链接读取内容 = %q, want %q", string(data), "content")
		}
	})

	t.Run("linkfile src 为当前目录", func(t *testing.T) {
		eng := newEngine()
		p := newProject(nil, []project.LinkFile{{Src: ".", Dest: "rootlink"}})
		if err := eng.processLinkAndCopyFiles(p); err != nil {
			t.Fatalf("处理 src=. 的 linkfile 返回错误: %v", err)
		}
		linkPath := filepath.Join(repoRoot, "rootlink")
		if got := resolveLinkTarget(t, linkPath); got != worktree {
			t.Fatalf("链接目标 = %q, want %q", got, worktree)
		}
		info, err := os.Stat(linkPath)
		if err != nil {
			t.Fatalf("stat 链接 failed: %v", err)
		}
		if !info.IsDir() {
			t.Fatalf("src=. 的链接应指向目录")
		}
	})

	t.Run("linkfile 绝对目标限制在 repoRoot", func(t *testing.T) {
		eng := newEngine()
		p := newProject(nil, []project.LinkFile{{Src: "src.txt", Dest: "/abs/link.txt"}})
		if err := eng.processLinkAndCopyFiles(p); err != nil {
			t.Fatalf("处理绝对路径 linkfile 返回错误: %v", err)
		}
		linkPath := filepath.Join(repoRoot, "abs", "link.txt")
		if got := resolveLinkTarget(t, linkPath); got != filepath.Join(worktree, "src.txt") {
			t.Fatalf("链接目标 = %q, want %q", got, filepath.Join(worktree, "src.txt"))
		}
	})

	t.Run("linkfile 越界目标", func(t *testing.T) {
		eng := newEngine()
		p := newProject(nil, []project.LinkFile{{Src: "src.txt", Dest: "../../pwn-link"}})
		err := eng.processLinkAndCopyFiles(p)
		if err == nil {
			t.Fatal("越界 linkfile 目标应返回错误")
		}
		if !strings.Contains(err.Error(), projectName) {
			t.Fatalf("错误信息应包含项目名 %q，实际: %v", projectName, err)
		}
		escaped := filepath.Clean(filepath.Join(repoRoot, "../../pwn-link"))
		if _, statErr := os.Lstat(escaped); !os.IsNotExist(statErr) {
			t.Fatalf("越界链接 %s 不应被创建 (err=%v)", escaped, statErr)
		}
	})

	t.Run("linkfile 中间符号链接", func(t *testing.T) {
		eng := newEngine()
		p := newProject(nil, []project.LinkFile{{Src: "src.txt", Dest: "evil/link"}})
		if err := eng.processLinkAndCopyFiles(p); err == nil {
			t.Fatal("经过符号链接的 linkfile 目标应返回错误")
		}
		if _, statErr := os.Lstat(filepath.Join(outside, "link")); !os.IsNotExist(statErr) {
			t.Fatalf("不应通过符号链接在 repoRoot 之外创建链接 (err=%v)", statErr)
		}
	})

	t.Run("linkfile 目标符号链接被安全重建", func(t *testing.T) {
		eng := newEngine()
		p := newProject(nil, []project.LinkFile{{Src: "src.txt", Dest: "destlink"}})
		if err := eng.processLinkAndCopyFiles(p); err != nil {
			t.Fatalf("重建已存在的链接返回错误: %v", err)
		}
		linkPath := filepath.Join(repoRoot, "destlink")
		if got := resolveLinkTarget(t, linkPath); got != filepath.Join(worktree, "src.txt") {
			t.Fatalf("链接目标 = %q, want %q", got, filepath.Join(worktree, "src.txt"))
		}
		data, err := os.ReadFile(filepath.Join(outside, "target.txt"))
		if err != nil {
			t.Fatalf("读取原链接目标 failed: %v", err)
		}
		if string(data) != "original" {
			t.Fatalf("原链接目标被修改，内容 = %q", string(data))
		}
	})

	t.Run("repoRoot 不被写回", func(t *testing.T) {
		l := logpkg.NewDefaultLogger()
		l.SetLevel(logpkg.LogLevelError)
		eng := &Engine{logger: l, manifest: &manifest.Manifest{Topdir: repoRoot}}
		p := newProject([]project.CopyFile{{Src: "src.txt", Dest: "no-writeback.txt"}}, nil)
		if err := eng.processLinkAndCopyFiles(p); err != nil {
			t.Fatalf("处理项目返回错误: %v", err)
		}
		if eng.repoRoot != "" {
			t.Fatalf("processLinkAndCopyFiles 不应写回 e.repoRoot: got %q", eng.repoRoot)
		}
		if _, err := os.Stat(filepath.Join(repoRoot, "no-writeback.txt")); err != nil {
			t.Fatalf("应使用 manifest.Topdir 作为仓库根目录: %v", err)
		}
	})

	t.Run("linkfile 通配符 src 链接所有匹配文件", func(t *testing.T) {
		mustWriteFile(t, filepath.Join(worktree, "glob", "a.h"), "a")
		mustWriteFile(t, filepath.Join(worktree, "glob", "b.h"), "b")
		eng := newEngine()
		p := newProject(nil, []project.LinkFile{{Src: "glob/*.h", Dest: "includedir"}})
		if err := eng.processLinkAndCopyFiles(p); err != nil {
			t.Fatalf("处理通配符 linkfile 返回错误: %v", err)
		}
		for _, name := range []string{"a.h", "b.h"} {
			linkPath := filepath.Join(repoRoot, "includedir", name)
			if got := resolveLinkTarget(t, linkPath); got != filepath.Join(worktree, "glob", name) {
				t.Errorf("%s 链接目标 = %q, want %q", name, got, filepath.Join(worktree, "glob", name))
			}
		}
	})
}
