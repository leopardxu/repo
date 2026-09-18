# 贡献指南 Contributing to repo-go

感谢关注 repo-go！欢迎通过 Issue 讨论问题、提交 Pull Request 贡献代码。
Thanks for your interest — issues and pull requests are welcome.（PR 请用英文或中文均可 / PRs in English or Chinese are both fine.）

## 开发环境 / Development Setup

| 要求 / Requirement | 说明 / Notes |
| --- | --- |
| Go **1.23+** | `go.mod` 声明 `go 1.23.0`，请勿假设更新的语言特性 |
| git | repo-go 通过执行系统 `git` 完成实际操作，运行与测试均需要 |
| golangci-lint v2.x | 可选，`make lint` 使用；v1 格式配置不兼容 |

```bash
git clone https://github.com/leopardxu/repo-go.git
cd repo-go
make build   # 产物 bin/repo
make test    # go test -v -race ./...
```

## 提交前的门禁 / Required Checks

所有门禁必须全绿，否则 PR 无法合并：

```bash
gofmt -l .          # 必须无输出
go vet ./...        # 必须无告警
go test -race ./... # 必须全部通过（CI 恒开竞态检测）
go build ./cmd/repo # 必须可构建
```

## 代码规范 / Code Conventions

1. **git 访问只走 `internal/git.Runner`**：禁止在 `git` 包之外直接 `exec.Command("git", ...)`。Runner 集中处理重试、trace、并发与超时，保证可测试性。
2. **语义对齐上游 Google `repo`**：除 `init` 外，所有子命令的用户可见行为必须与上游 Python `repo` 一致（`init` 是单二进制化的例外，不 clone launcher 仓库）。改动命令行为前请先核对上游语义。
3. **错误处理**：返回的 error 必须检查；包装用 `fmt.Errorf("context: %w", err)`；判等用 `errors.Is` / `errors.As`；错误类型遵循 `XxxError struct{Op, Path, Err}` + `Unwrap()` 形态；error 要么 log 要么 return，二选一；错误串小写。
4. **并发**：批量并发必须走 `internal/workerpool` 或显式设上限；外部调用（git/HTTP/SSH）必须带 timeout。
5. **日志**：每个包持有包级 `log logger.Logger` 并暴露 `SetLogger(...)`，新包沿用该模式。
6. **注释用中文**：跟随现有代码风格（回答英文 PR 评审没问题，代码注释保持中文）。
7. **环境变量只用 `REPO_` 前缀**；cobra 旗标先查 `cmd/repo/commands/common_options.go` 是否已有，避免重复定义。
8. **slice/map 显式初始化，禁 nil；禁 `init()`**。

## 测试规范 / Testing

- `foo.go` 对应 `foo_test.go`，同名同序；优先扩展现有 `_test.go`。
- table-driven 风格 + `t.Run(tt.name, ...)` 命名子测试。
- 测试内用 `git.Runner` 接口 mock git，不启动真实子进程。
- 测试行为与边界，不测实现细节；测试必须确定性、可并行、可重复。

## 提交信息 / Commit Messages

遵循 [Conventional Commits](https://www.conventionalcommits.org/zh-hans/)，中文摘要：

```
fix(sync): 确保所有命令在repo根目录执行
feat(manifest): 支持自定义XML属性
```

## 提交流程 / Submitting a PR

1. 从 `main` 拉出特性分支（如 `feat/sync-depth`）；
2. 小步提交，保持每个 commit 可构建；
3. PR 描述请说明：动机、行为变化（是否对齐上游语义）、验证方式；
4. CI（gofmt / vet / golangci-lint / `go test -race`）通过后等待评审。

## 行为准则 / Code of Conduct

保持专业与友善。请对不同的技术观点保持开放，评审意见针对代码而非个人。
Keep discussions professional and respectful; critique code, not people.
