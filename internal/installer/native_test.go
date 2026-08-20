package installer

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUpstreamInstallerPathIsProjectAndCommitScoped(t *testing.T) {
	commit := strings.Repeat("a", 40)
	path, err := upstreamInstallerFile("cottendns", commit)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(upstreamStateDir, "cottendns", commit+".sh")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	for _, bad := range []string{"../bad", "abc", strings.Repeat("z", 40)} {
		if _, err := upstreamInstallerFile("cottendns", bad); err == nil {
			t.Fatalf("unsafe commit accepted: %q", bad)
		}
	}
	if _, err := upstreamInstallerFile("unknown", commit); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestPendingManifestCannotReplaceCompletedManifest(t *testing.T) {
	completed, err := manifestFile("cottendns")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := pendingManifestFile("cottendns")
	if err != nil {
		t.Fatal(err)
	}
	if completed == pending || !strings.HasSuffix(pending, ".pending.json") {
		t.Fatalf("completed=%q pending=%q", completed, pending)
	}
}
