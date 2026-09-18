package commands

import "github.com/spf13/cobra"

// CommonManifestOptions 包含清单相关的选项
type CommonManifestOptions struct {
	Groups                   string
	Platform                 bool
	OuterManifest            bool
	NoOuterManifest          bool
	ThisManifestOnly         bool
	AllManifests             bool
	RevisionAsHEAD           bool
	OutputFile               string
	SuppressUpstreamRevision bool
	SuppressDestBranch       bool
	Snapshot                 bool
	NoCloneBundle            bool
	JSONOutput               bool
	PrettyOutput             bool
	NoLocalManifests         bool
}

// AddManifestFlags 添加多清单选项到命令。
//
// 注意：repo-go 在 init 时已把所有 <include> 合并成扁平的 .repo/manifest.xml，
// 未保留持久 manifest 树，故这些子清单作用域 flag（--outer-manifest /
// --this-manifest-only / --all-manifests）当前在合并后的单一清单上退化为整体作用：
// 任意取值均作用于全部已合并项目。各命令接受这些 flag 以保持参数兼容，
// 但不按 manifest 层做差异化过滤。如需真正的按层作用域，需改造 manifest 包
// 在合并时保留 include 树（架构改动，另行评估）。
func AddManifestFlags(cmd *cobra.Command, opts *CommonManifestOptions) {
	cmd.Flags().BoolVar(&opts.OuterManifest, "outer-manifest", false, "operate starting at the outermost manifest")
	cmd.Flags().BoolVar(&opts.NoOuterManifest, "no-outer-manifest", false, "do not operate on outer manifests")
	cmd.Flags().BoolVar(&opts.ThisManifestOnly, "this-manifest-only", false, "only operate on this (sub)manifest")
	cmd.Flags().BoolVar(&opts.AllManifests, "all-manifests", false, "operate on this manifest and its submanifests")
}
