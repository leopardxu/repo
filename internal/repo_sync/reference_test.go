package repo_sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
	"github.com/leopardxu/repo-go/internal/project"
)

// writeGitLayout 在 dir 下写入最小 git 仓库布局（HEAD/objects/refs）。
func writeGitLayout(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0755); err != nil {
		t.Fatalf("mkdir objects failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0755); err != nil {
		t.Fatalf("mkdir refs failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatalf("write HEAD failed: %v", err)
	}
}

// TestResolveReferenceRepo 验证参考仓库路径解析对齐上游 repo 语义：
// reference 为仓库本身时原样返回；为目录时按项目名探测 <name>.git 与 <name>；
// 找不到时返回空串，由调用方降级为不使用参考仓库。
func TestResolveReferenceRepo(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, root string) (reference string, project string)
		want    func(root string) string
	}{
		{
			name: "reference itself is a bare repo",
			prepare: func(t *testing.T, root string) (string, string) {
				ref := filepath.Join(root, "mirror.git")
				writeGitLayout(t, ref)
				return ref, "platform/build"
			},
			want: func(root string) string {
				return filepath.Join(root, "mirror.git")
			},
		},
		{
			name: "reference itself is a non-bare repo",
			prepare: func(t *testing.T, root string) (string, string) {
				ref := filepath.Join(root, "repo")
				writeGitLayout(t, filepath.Join(ref, ".git"))
				return ref, "platform/build"
			},
			want: func(root string) string {
				return filepath.Join(root, "repo")
			},
		},
		{
			name: "directory resolves project via name dot git",
			prepare: func(t *testing.T, root string) (string, string) {
				ref := filepath.Join(root, "mirrors", "platform", "build.git")
				writeGitLayout(t, ref)
				return filepath.Join(root, "mirrors"), "platform/build"
			},
			want: func(root string) string {
				return filepath.Join(root, "mirrors", "platform", "build.git")
			},
		},
		{
			name: "directory resolves project via plain name",
			prepare: func(t *testing.T, root string) (string, string) {
				ref := filepath.Join(root, "mirrors", "platform", "build")
				writeGitLayout(t, ref)
				return filepath.Join(root, "mirrors"), "platform/build"
			},
			want: func(root string) string {
				return filepath.Join(root, "mirrors", "platform", "build")
			},
		},
		{
			name: "directory prefers name dot git over plain name",
			prepare: func(t *testing.T, root string) (string, string) {
				writeGitLayout(t, filepath.Join(root, "mirrors", "build.git"))
				writeGitLayout(t, filepath.Join(root, "mirrors", "build"))
				return filepath.Join(root, "mirrors"), "build"
			},
			want: func(root string) string {
				return filepath.Join(root, "mirrors", "build.git")
			},
		},
		{
			name: "missing candidate returns empty",
			prepare: func(t *testing.T, root string) (string, string) {
				if err := os.MkdirAll(filepath.Join(root, "mirrors"), 0755); err != nil {
					t.Fatalf("mkdir mirrors failed: %v", err)
				}
				return filepath.Join(root, "mirrors"), "platform/build"
			},
			want: func(string) string { return "" },
		},
		{
			name: "empty reference returns empty",
			prepare: func(_ *testing.T, _ string) (string, string) {
				return "", "platform/build"
			},
			want: func(string) string { return "" },
		},
		{
			name: "empty project name returns empty",
			prepare: func(_ *testing.T, root string) (string, string) {
				return filepath.Join(root, "mirrors"), ""
			},
			want: func(string) string { return "" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			reference, projName := tt.prepare(t, root)
			got := resolveReferenceRepo(reference, projName)
			if want := tt.want(root); got != want {
				t.Errorf("resolveReferenceRepo(%q, %q) = %q, want %q", reference, projName, got, want)
			}
		})
	}
}

// TestBuildCloneArgsReference 验证 clone 参数构造：
// --reference 按项目名解析到 DIR/<name>.git，且 --dissociate 仅在开启时追加。
func TestBuildCloneArgsReference(t *testing.T) {
	tests := []struct {
		name           string
		dissociate     bool
		wantDissociate bool
	}{
		{
			name:           "reference without dissociate",
			dissociate:     false,
			wantDissociate: false,
		},
		{
			name:           "reference with dissociate",
			dissociate:     true,
			wantDissociate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			refRepo := filepath.Join(root, "mirrors", "build.git")
			writeGitLayout(t, refRepo)

			e := &Engine{
				logger: logger.NewDefaultLogger(),
				options: &Options{
					Reference:  filepath.Join(root, "mirrors"),
					Dissociate: tt.dissociate,
					Quiet:      true,
				},
			}
			p := &project.Project{
				Name:       "build",
				RemoteName: "origin",
				Worktree:   filepath.Join(root, "work", "build"),
			}

			args := e.buildCloneArgs(p, "https://example.com/build.git", false)

			refIdx := indexOfArg(args, "--reference")
			if refIdx < 0 || refIdx+1 >= len(args) {
				t.Fatalf("clone args missing --reference value: %v", args)
			}
			// --reference 的值必须是按项目名解析出的子仓库，而非参考目录本身
			if args[refIdx+1] != refRepo {
				t.Errorf("--reference value = %q, want %q", args[refIdx+1], refRepo)
			}
			if gotDissociate := containsArg(args, "--dissociate"); gotDissociate != tt.wantDissociate {
				t.Errorf("--dissociate present = %v, want %v (args: %v)", gotDissociate, tt.wantDissociate, args)
			}
		})
	}
}

// TestBuildCloneArgsNoReference 验证未命中参考仓库时降级：不追加 --reference/--dissociate。
func TestBuildCloneArgsNoReference(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mirrors"), 0755); err != nil {
		t.Fatalf("mkdir mirrors failed: %v", err)
	}

	e := &Engine{
		logger: logger.NewDefaultLogger(),
		options: &Options{
			Reference:  filepath.Join(root, "mirrors"),
			Dissociate: true,
			Quiet:      true,
		},
	}
	p := &project.Project{
		Name:       "build",
		RemoteName: "origin",
		Worktree:   filepath.Join(root, "work", "build"),
	}

	args := e.buildCloneArgs(p, "https://example.com/build.git", false)
	if containsArg(args, "--reference") {
		t.Errorf("--reference should be absent when no repo matches, args: %v", args)
	}
	if containsArg(args, "--dissociate") {
		t.Errorf("--dissociate should be absent without --reference, args: %v", args)
	}
}

// containsArg 判断 args 中是否存在精确匹配的 flag。
func containsArg(args []string, target string) bool {
	return indexOfArg(args, target) >= 0
}

// indexOfArg 返回 target 在 args 中的下标，不存在时返回 -1。
func indexOfArg(args []string, target string) int {
	for i, a := range args {
		if a == target {
			return i
		}
	}
	return -1
}
