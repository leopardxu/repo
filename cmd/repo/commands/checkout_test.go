package commands

import (
	"bytes"
	"runtime"
	"testing"
)

// TestNormalizeCheckoutJobs 验证 checkout 并发数归一化：
// 正数保持原值；0/负数回退到 CPU 数且至少为 1（避免 engine 内 make(chan, <=0) panic）。
func TestNormalizeCheckoutJobs(t *testing.T) {
	tests := []struct {
		name string
		jobs int
	}{
		{name: "正数保持", jobs: 1},
		{name: "正数保持多并发", jobs: 8},
		{name: "零回退", jobs: 0},
		{name: "负数回退", jobs: -3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCheckoutJobs(tt.jobs)
			if got < 1 {
				t.Fatalf("normalizeCheckoutJobs(%d) = %d, want >= 1", tt.jobs, got)
			}
			if tt.jobs > 0 && got != tt.jobs {
				t.Errorf("normalizeCheckoutJobs(%d) = %d, want %d", tt.jobs, got, tt.jobs)
			}
			if tt.jobs <= 0 {
				want := runtime.NumCPU()
				if want < 1 {
					want = 1
				}
				if got != want {
					t.Errorf("normalizeCheckoutJobs(%d) = %d, want %d", tt.jobs, got, want)
				}
			}
		})
	}
}

// TestPrintCheckoutSummary 验证检出摘要写入给定 writer（stdout payload），
// 且内容不受 -q 影响（helper 本身无 quiet 分支）。
func TestPrintCheckoutSummary(t *testing.T) {
	var buf bytes.Buffer
	printCheckoutSummary(&buf, "topic", 3, 1)
	want := "检出分支 'topic' 完成: 3 成功, 1 failed\n"
	if buf.String() != want {
		t.Fatalf("summary = %q, want %q", buf.String(), want)
	}
}
