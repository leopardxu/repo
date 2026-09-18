package repo_sync

import (
	"time"

	"github.com/leopardxu/repo-go/internal/config"
)

// Options 包含同步选项
type Options struct {
	NetworkOnly            bool
	LocalOnly              bool
	CurrentBranch          bool
	Detach                 bool
	Force                  bool
	NoTags                 bool
	Prune                  bool
	Jobs                   int
	JobsNetwork            int
	JobsCheckout           int
	SmartSync              bool
	SmartTag               string
	UseSuperproject        bool
	HyperSync              bool
	Verbose                bool
	Quiet                  bool
	Tags                   bool
	GitLFS                 bool // 添加 GitLFS 字段
	ForceSync              bool
	ForceOverwrite         bool
	ForceRemoveDirty       bool // 添加 ForceRemoveDirty 字段
	ForceBroken            bool // 继续同步即使项目已损坏
	FailFast               bool
	HTTPTimeout            time.Duration
	ManifestServerUsername string
	ManifestServerPassword string
	ManifestServerURL      string         // manifest服务器URL
	NoManifestServer       bool           // 禁用manifest服务器
	Groups                 []string       // 修改为字符串数组
	Debug                  bool           // 添加 Debug 字段
	OptimizedFetch         bool           // 添加 OptimizedFetch 字段
	RetryFetches           int            // 添加 RetryFetches 字段
	NoCloneBundle          bool           // 添加 NoCloneBundle 字段
	Depth                  int            // 添加 Depth 字段
	FetchSubmodules        bool           // 添加 FetchSubmodules 字段
	NoManifestUpdate       bool           // 添加 NoManifestUpdate 字段
	AutoGC                 bool           // sync后自动运行git gc
	DryRun                 bool           // 添加 DryRun 字段，用于模拟执行但不实际修改
	Config                 *config.Config // 添加 Config 字段，用于存储配置信息
	DefaultRemote          string         // 添加 DefaultRemote 字段，用于指定默认远程
	Reference              string         // 本地参考仓库路径：仓库本身或存放多个参考仓库的目录
	Dissociate             bool           // 克隆后解除对参考仓库的 alternates 依赖（git clone --dissociate）

	// cherry-pick 选项（repo cherry-pick 扩展命令）
	CherryPickX  bool // -x：记录被 cherry-pick 的原始提交
	CherryPickFF bool // --ff：允许 fast-forward
}
