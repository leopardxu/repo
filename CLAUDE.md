# CLAUDE.md - repo-go

This file guides AI agents (Claude Code) working in this repository. Read it alongside the user's global instructions.

## What this project is

`repo-go` is a Go reimplementation of Google's `repo` (the multi-repository Git management tool, originally built for Android). It manages many Git repositories described by an XML **manifest** through a single `repo` binary with subcommands (`init`, `sync`, `start`, `upload`, `forall`, `smart_sync`, …).

- Module: `github.com/leopardxu/repo-go`
- Go version: **1.23.0** (toolchain `go1.23.6`) - declared in `go.mod`; do not assume newer language features.
- Single dependency surface: `github.com/spf13/cobra v1.7.0` (CLI) + `golang.org/x/sync v0.13.0`. The tool shells out to the system `git`, so a working `git` on PATH is a runtime requirement.
- Behavioral compatibility with the original Python `repo` is a design goal - when in doubt about how a subcommand should behave, mirror upstream `repo` semantics, not a freshly invented convention.
- **Exception: `repo init` differs from upstream.** repo-go is a single static binary; it does **not** clone a separate `repo` launcher repository (no `.repo/repo/` checkout, no `--repo-url`/`--repo-rev` self-update). `init` only clones the manifest repository and writes `.repo/config.json` + `.repo/manifest.xml`. All `--repo-url`, `--repo-rev`, `--no-repo-verify` flags are accepted but ignored (with a warning). Every other subcommand (`sync`, `start`, `status`, `upload`, `forall`, …) must produce the same user-visible behavior as upstream Google `repo`. When adding or modifying any non-init command, match upstream semantics exactly.

## Build, run, test

```bash
make build          # builds bin/repo.exe with version ldflags (Windows host)
make build-all      # cross-compile windows / linux / macos
make run            # go run ./cmd/repo/main.go
make test           # go test -v -race ./...
make vet            # go vet ./...
make lint           # golangci-lint run  (golangci-lint must be installed)
make clean          # rm bin/ coverage.txt
```

Version info is injected at build time via `-ldflags` into `main.version`, `main.commit`, `main.date` (declared in `cmd/repo/main.go`). `scripts/build.bat` is an alternative Windows build script (builds `bin/repo.exe` + `bin/repo-linux`).

Before declaring Go work done, run the global verification workflow and ensure it passes:

```bash
go mod tidy && go vet ./... && gofmt -l . && go test ./...
```

`gofmt -l .` must produce **no output**. Run Go commands through Git Bash on this Windows host; in Go source use forward-slash paths and `filepath.Join` for OS-specific paths.

## Architecture / layout

```
cmd/repo/
  main.go              # entry point: root cobra.Command, global flags, registers subcommands
  commands/            # one file per subcommand + common_options.go + common_utils.go
internal/
  config/              # Config + RepoConfig, GetRepoRoot(), .repo dir handling
  manifest/            # XML manifest parsing/merging/clone (Manifest, Remote, Project, ...). Supports custom XML attrs via CustomAttrs.
  project/             # Project model + Manager
  git/                 # git command wrapper: Runner interface + Repository
  repo_sync/           # sync engine: engine.go, sync.go, fetch.go, hypersync.go, smart_sync.go, superproject.go, retry.go, pathutil.go
  workerpool/          # bounded concurrency pool used by sync/forall
  logger/ color/ ui/ progress/  # output / logging / progress
  hook/                # repo-hooks support
  network/ ssh/        # HTTP(S) client, SSH proxy
docs/                  # plan.md (design), product.md (upstream analysis), custom_attributes.md
```

### Key seams & patterns (match these when extending)

- **Git access goes through `internal/git.Runner`** (interface with `Run`, `RunInDir`, `RunWithTimeout`, `RunInDirWithTimeout`, plus `SetVerbose/...`). Never call `exec.Command("git", ...)` directly outside the `git` package - go through `Runner` so behavior (retry, trace, concurrency, timeout) is centralized and testable.
- **Logging**: every package holds a package-level `log logger.Logger` initialized to `logger.NewDefaultLogger()` and exposes a `SetLogger(...)`. The root sets the global logger via `logger.SetGlobalLogger`. Levels: `Error < Warn < Info < Debug < Trace`; `--trace` sets `LogLevelTrace` and exports `REPO_TRACE=1`.
- **Concurrency**: use `internal/workerpool` for bounded parallel work; respect `--jobs` / `--jobs-network` / `--jobs-checkout` in sync.
- **Error types** follow the `XxxError struct { Op, Path, Err }` shape with `Error()` and `Unwrap()` (see `manifest.ManifestError`, `config.ConfigError`, `git.GitCommandError`). Add `Unwrap()` so `errors.Is/As` work.
- **Commands chdir to the repo root** via `commands.EnsureRepoRoot` and restore with `commands.RestoreWorkDir`. The `.repo/` directory is the working/state root, just like upstream repo.
- **Cobra global flags** are defined in `cmd/repo/main.go` (`--trace`, `--color`, `--paginate`, `--no-pager`, `--event-log`, `--git-trace2-event-log`, `--submanifest-path`, …). Reuse existing options from `common_options.go` / helpers from `common_utils.go` before adding new ones.

### Runtime environment variables (relied on by code)

- `REPO_TRACE=1` - trace git command execution (`--trace` sets it).
- `REPO_LOG_FILE=<path>` - redirect debug logs to a file.
- `NO_COLOR=1` - disable color output (`--color never` sets it).
- `GODEBUG=http2debug=2,gctrace=1` - set by `--trace-go`.
- `REPO_MIRROR_LOCATION=<dir>` - `repo init --reference` fallback (cmd/repo/commands/init.go:133).
- `REPO_GIT_LFS=<bool>` - `repo init --git-lfs` fallback (cmd/repo/commands/init.go:138).
- `REPO_SELFUPDATE_URL=<url>` - default download URL for `repo selfupdate` (cmd/repo/commands/selfupdate.go:54).
- `REPO_SYNC_TARGET=<target>` - smart_sync target resolution prefers this, falling back to the AOSP convention `TARGET_PRODUCT`/`TARGET_BUILD_VARIANT`.
- `REPO_MANIFEST_URL`, `REPO_MANIFEST_BRANCH`, `REPO_MANIFEST_NAME`, `REPO_GROUPS`, `REPO_PLATFORM`, `REPO_VERBOSE`, `REPO_QUIET`, `REPO_MIRROR`, `REPO_ARCHIVE`, `REPO_DEPTH` - config overrides read by `config.ApplyEnvironment()`.

> **Note:** The old `GOGO_*` prefix is **no longer supported**. All environment variables use the `REPO_` prefix exclusively.

## Conventions

- **Comments are written in Chinese (中文)** throughout the codebase (e.g. `// 包级别的日志记录器`, `// 确保路径使用正确的分隔符`). Match this: write new code comments in Chinese to stay consistent with the surrounding file.
- Commit messages use Conventional Commits, often with a scope and a Chinese summary (e.g. `fix(repo): 确保所有命令在repo根目录执行...`, `feat: introduce manifest package...`).
- Tests live in `internal/repo_sync/`, `internal/workerpool/`, `internal/manifest/`, `internal/config/`, `internal/git/`, `internal/project/`, and `cmd/repo/commands/`. When you touch logic in any package, prefer extending existing `_test.go` files and follow their table-driven style. Use the `Runner` interface to mock git in new tests.

## Things to verify before finishing a change

1. `go vet ./...` is clean and `gofmt -l .` is empty.
2. `go test ./...` passes (add/extend tests for `repo_sync` and `workerpool` logic).
3. New git access goes through `git.Runner`; new packages follow the `log` + `SetLogger` pattern; errors are typed with `Unwrap()`.
4. New comments are in Chinese and match surrounding tone.
5. If a command changes working-directory behavior, it still calls `EnsureRepoRoot` / `RestoreWorkDir`.
