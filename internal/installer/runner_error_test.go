package installer

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestOSRunnerErrorCarriesCommandAndOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	err := OSRunner{}.Run(context.Background(), "sh", []string{"-c", "echo boom >&2; exit 1"}, "/", false)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "sh -c") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error lost command or output: %v", err)
	}
}
