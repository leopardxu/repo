package commands

import (
	"encoding/json"
	"testing"
)

// TestChangeInfo_ParseCurrentPatchSet 验证 Gerrit 查询返回的 currentPatchSet.number 被正确解析，
// 用于 repo download 在未指定 patchset 时取最新补丁集（High：原实现硬编码 patchset="1" 取到旧补丁集）。
func TestChangeInfo_ParseCurrentPatchSet(t *testing.T) {
	data := `{"project":"platform/build","id":"I1234567890abcdef","number":42,"currentPatchSet":{"number":3,"revision":"abc"}}`

	var c ChangeInfo
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if c.Number != 42 {
		t.Errorf("expected Number=42, got %d", c.Number)
	}
	if c.CurrentPatchSet.Number != 3 {
		t.Errorf("expected CurrentPatchSet.Number=3, got %d", c.CurrentPatchSet.Number)
	}
}
