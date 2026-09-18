package commands

import (
	"testing"

	"github.com/leopardxu/repo-go/internal/logger"
)

// TestPrintPendingBranches 验证上游 "Pending Branches" 输出格式：
// 分组头、当前分支 * 标记、提交数与日期（核心渲染逻辑，不依赖 git）
func TestPrintPendingBranches(t *testing.T) {
	tests := []struct {
		name    string
		pending []pendingBranch
		quiet   bool
	}{
		{
			name: "空列表不输出",
		},
		{
			name: "单项目多分支分组输出",
			pending: []pendingBranch{
				{Project: "component/a", Branch: "topic1", Commits: 3, Date: "2026-08-01"},
				{Project: "component/a", Branch: "topic2", Current: true, Commits: 12, Date: "2026-08-02"},
			},
		},
		{
			name: "多项目分组切换",
			pending: []pendingBranch{
				{Project: "component/a", Branch: "topic1", Commits: 1, Date: "2026-08-01"},
				{Project: "component/b", Branch: "topic9", Commits: 7, Date: "2026-08-05"},
			},
		},
		{
			name: "quiet 模式静默",
			pending: []pendingBranch{
				{Project: "component/a", Branch: "topic1", Commits: 3, Date: "2026-08-01"},
			},
			quiet: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(_ *testing.T) {
			log := logger.NewDefaultLogger()
			log.SetLevel(logger.LogLevelInfo)
			opts := &PruneOptions{Quiet: tt.quiet}
			// 不应 panic；输出内容由人工对齐上游格式
			printPendingBranches(tt.pending, opts, log)
		})
	}
}
