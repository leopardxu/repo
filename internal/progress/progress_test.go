package progress

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNewConsoleReporter(t *testing.T) {
	r := NewConsoleReporter()
	if r == nil {
		t.Fatal("NewConsoleReporter returned nil")
	}
}

func TestConsoleReporterStart(t *testing.T) {
	r := &ConsoleReporter{writer: &bytes.Buffer{}, enabled: true}
	r.Start(10)

	if r.total != 10 {
		t.Errorf("total = %d, want 10", r.total)
	}
	if r.current != 0 {
		t.Errorf("current = %d, want 0", r.current)
	}
}

func TestConsoleReporterUpdate(t *testing.T) {
	var buf bytes.Buffer
	r := &ConsoleReporter{writer: &buf, enabled: true}
	r.Start(10)

	r.Update(5, "processing")
	// Update should not panic and should write something
	if buf.Len() == 0 {
		t.Error("Update should produce output")
	}
}

func TestConsoleReporterFinish(_ *testing.T) {
	var buf bytes.Buffer
	r := &ConsoleReporter{writer: &buf, enabled: true}
	r.Start(10)
	r.Update(5, "processing")
	r.Finish()
	// Should not panic
}

func TestConsoleReporterDisabled(t *testing.T) {
	var buf bytes.Buffer
	r := &ConsoleReporter{writer: &buf, enabled: false}
	r.Start(10)
	r.Update(5, "processing")
	r.Finish()
	// Disabled reporter should not produce output
	if buf.Len() > 0 {
		t.Error("Disabled reporter should not produce output")
	}
}

func TestNewProgress(t *testing.T) {
	p := NewProgress("test", 10, false)
	if p == nil {
		t.Fatal("NewProgress returned nil")
	}
}

func TestNewProgressQuiet(t *testing.T) {
	p := NewProgress("test", 10, true)
	if p == nil {
		t.Fatal("NewProgress returned nil with quiet=true")
	}
}

func TestProgressUpdate(_ *testing.T) {
	p := NewProgress("test", 10, true)
	p.Update("item-1")
	p.Update("item-2")
	// Should not panic
}

func TestProgressFinish(_ *testing.T) {
	p := NewProgress("test", 10, true)
	p.Update("item-1")
	p.Finish("")
	// Should not panic
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m30s"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if !strings.Contains(got, "s") {
			t.Errorf("formatDuration(%v) = %q, should contain time unit", tt.d, got)
		}
	}
}

func TestClampCurrent(t *testing.T) {
	tests := []struct {
		name    string
		current int
		total   int
		want    int
	}{
		{"in range", 3, 10, 3},
		{"above total", 11, 10, 10},
		{"negative", -1, 10, 0},
		{"zero total", 5, 0, 0},
		{"negative total", 5, -2, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampCurrent(tt.current, tt.total); got != tt.want {
				t.Errorf("clampCurrent(%d, %d) = %d, want %d", tt.current, tt.total, got, tt.want)
			}
		})
	}
}

func TestBarFilled(t *testing.T) {
	tests := []struct {
		name    string
		current int
		total   int
		want    int
	}{
		{"start", 0, 10, 0},
		{"complete", 10, 10, progressBarWidth},
		{"above total", 20, 10, progressBarWidth},
		{"negative current", -5, 10, 0},
		{"half", 5, 10, progressBarWidth / 2},
		{"zero total", 1, 0, 0},
		{"negative total", 1, -1, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := barFilled(tt.current, tt.total, progressBarWidth); got != tt.want {
				t.Errorf("barFilled(%d, %d, %d) = %d, want %d",
					tt.current, tt.total, progressBarWidth, got, tt.want)
			}
		})
	}
}

func TestConsoleReporterUpdateIsAbsoluteAndClamped(t *testing.T) {
	var buf bytes.Buffer
	r := &ConsoleReporter{writer: &buf, enabled: true}
	r.Start(10)

	r.Update(7, "")
	if r.current != 7 {
		t.Errorf("current = %d, want 7", r.current)
	}

	// Update 是绝对计数，不回退递增
	r.Update(3, "")
	if r.current != 3 {
		t.Errorf("current = %d, want 3", r.current)
	}

	r.Update(99, "")
	if r.current != 10 {
		t.Errorf("超限 current = %d, want 10", r.current)
	}
	if !strings.Contains(buf.String(), "10/10") {
		t.Errorf("输出应包含 10/10, got %q", buf.String())
	}
}

func TestConsoleReporterOutOfRangeDoesNotPanic(t *testing.T) {
	tests := []struct {
		name   string
		total  int
		update int
	}{
		{"current above total", 10, 20},
		{"negative current", 10, -5},
		{"zero total", 0, 5},
		{"negative total", -3, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := &ConsoleReporter{writer: &buf, enabled: true}
			r.Start(tt.total)
			r.Update(tt.update, "")
			r.Finish()
		})
	}
}

func TestConsoleReporterUpdateAfterFinish(t *testing.T) {
	var buf bytes.Buffer
	r := &ConsoleReporter{writer: &buf, enabled: true}
	r.Start(5)
	r.Update(5, "")
	r.Finish()

	// Finish 之后继续 Update 不应 panic
	r.Update(9, "after finish")
	if r.current != 5 {
		t.Errorf("current = %d, want 5", r.current)
	}
	r.Update(0, "")
	if r.current != 0 {
		t.Errorf("current = %d, want 0", r.current)
	}
}

func TestProgressClampsAfterFinish(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress("test", 5, false)
	p.writer = &buf

	for range 5 {
		p.Update("")
	}
	p.Finish("done")

	// Finish 之后继续 Update 不应 panic
	p.Update("after finish")
	if got := p.GetCurrent(); got != 5 {
		t.Errorf("GetCurrent() = %d, want 5", got)
	}
	if !strings.Contains(buf.String(), "5/5") {
		t.Errorf("输出应包含 5/5, got %q", buf.String())
	}
}

func TestProgressUpdateBeyondTotal(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress("test", 2, false)
	p.writer = &buf

	for range 5 {
		p.Update("")
	}
	if got := p.GetCurrent(); got != 2 {
		t.Errorf("GetCurrent() = %d, want 2", got)
	}
}

func TestProgressZeroTotalDoesNotPanic(t *testing.T) {
	p := NewProgress("test", 0, false)
	p.Update("")
	p.Finish("")

	p.SetTotal(-3)
	p.Update("")
	if got := p.GetCurrent(); got != 0 {
		t.Errorf("GetCurrent() = %d, want 0", got)
	}
}

func TestProgressSetTotalClampsCurrent(t *testing.T) {
	p := NewProgress("test", 10, false)
	for range 8 {
		p.Update("")
	}

	p.SetTotal(3)
	if got := p.GetCurrent(); got != 3 {
		t.Errorf("SetTotal 后 GetCurrent() = %d, want 3", got)
	}
}

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"short unchanged", "abc", 10, "abc"},
		{"exact length unchanged", "abcde", 5, "abcde"},
		{"ascii truncation", "abcdefghij", 8, "abcde..."},
		{"chinese keeps rune boundary", strings.Repeat("中", 10), 10, "中中..."},
		{"zero max", "abc", 0, ""},
		{"tiny limit ascii", "abc", 2, "ab"},
		{"tiny limit chinese", "中中", 2, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateUTF8(tt.s, tt.max)
			if got != tt.want {
				t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateUTF8(%q, %d) 产生非法 UTF-8: %q", tt.s, tt.max, got)
			}
			if tt.max > 0 && len(got) > tt.max {
				t.Errorf("truncateUTF8(%q, %d) 长度 %d 超过上限", tt.s, tt.max, len(got))
			}
		})
	}
}

func TestProgressDisplayDoesNotSplitMultibyte(t *testing.T) {
	var buf bytes.Buffer
	p := NewProgress("中文标题", 10, false)
	p.writer = &buf

	// 超长中文消息会触发截断，切点必须落在 rune 边界
	p.Update(strings.Repeat("中文", 100))

	out := buf.String()
	if !utf8.ValidString(out) {
		t.Fatalf("进度输出被切坏，非合法 UTF-8: %q", out)
	}
	line := strings.TrimPrefix(out, "\r")
	if len(line) > maxDisplayLen {
		t.Errorf("单行输出长度 = %d, want <= %d", len(line), maxDisplayLen)
	}
}

func TestProgressConcurrentSetTotalAndUpdate(t *testing.T) {
	p := NewProgress("test", 10, false)
	p.writer = io.Discard

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			p.Update("")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			p.SetTotal(i % 20)
		}
	}()
	wg.Wait()

	if got := p.GetTotal(); got < 0 || got >= 20 {
		t.Errorf("GetTotal() = %d, 超出 SetTotal 的取值区间", got)
	}
}
