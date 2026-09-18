package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
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
