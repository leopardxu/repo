package color

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewColoring(t *testing.T) {
	c := NewColoring(true)
	if c == nil {
		t.Fatal("NewColoring returned nil")
	}
	if !c.enabled {
		t.Error("Coloring should be enabled")
	}
}

func TestNewColoringDisabled(t *testing.T) {
	c := NewColoring(false)
	if c.enabled {
		t.Error("Coloring should be disabled")
	}
}

func TestSetEnabled(t *testing.T) {
	c := NewColoring(false)
	c.SetEnabled(true)
	if !c.enabled {
		t.Error("SetEnabled(true) should enable coloring")
	}
	c.SetEnabled(false)
	if c.enabled {
		t.Error("SetEnabled(false) should disable coloring")
	}
}

func TestColorText(t *testing.T) {
	c := NewColoring(true)
	result := c.Colorize("hello", Red)
	if !strings.Contains(result, "hello") {
		t.Errorf("Colorize should contain text, got: %s", result)
	}
	if !strings.Contains(result, "\033[31m") {
		t.Errorf("Colorize should contain red color code, got: %s", result)
	}
	if !strings.Contains(result, "\033[0m") {
		t.Errorf("Colorize should contain reset code, got: %s", result)
	}
}

func TestColorTextDisabled(t *testing.T) {
	c := NewColoring(false)
	result := c.Colorize("hello", Red)
	// When disabled, should return plain text without ANSI codes
	if result != "hello" {
		t.Errorf("Colorize when disabled should return plain text, got: %s", result)
	}
}

func TestPrintf(t *testing.T) {
	var buf bytes.Buffer
	c := NewColoring(true)
	c.SetWriter(&buf)

	c.Printf("test %s", Green, "world")
	output := buf.String()
	if !strings.Contains(output, "test world") {
		t.Errorf("Printf output should contain text, got: %s", output)
	}
}

func TestShouldUseColor(t *testing.T) {
	// Test with explicit mode
	if ShouldUseColor("never") {
		t.Error("ShouldUseColor('never') should be false")
	}
	if !ShouldUseColor("always") {
		t.Error("ShouldUseColor('always') should be true")
	}
	// auto depends on terminal, just verify it doesn't panic
	_ = ShouldUseColor("auto")
	_ = ShouldUseColor("")
}

func TestColorValues(t *testing.T) {
	// Verify color map has entries for basic colors
	if colorMap[Red] != "\033[31m" {
		t.Errorf("Red color code = %q, want \\033[31m", colorMap[Red])
	}
	if colorMap[Green] != "\033[32m" {
		t.Errorf("Green color code = %q, want \\033[32m", colorMap[Green])
	}
	if colorMap[Reset] != "\033[0m" {
		t.Errorf("Reset color code = %q, want \\033[0m", colorMap[Reset])
	}
}

func TestNewBranchColoring(t *testing.T) {
	c := NewBranchColoring(true)
	if c == nil {
		t.Fatal("NewBranchColoring returned nil")
	}
}

func TestNewBranchColoringDisabled(t *testing.T) {
	c := NewBranchColoring(false)
	if c == nil {
		t.Fatal("NewBranchColoring(false) returned nil")
	}
}
