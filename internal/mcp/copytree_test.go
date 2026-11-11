package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// fetch_file copies into the user's project: names tools or agents act on
// are renamed at every depth and nothing is executable, even for files
// that were received before ferry renamed them on arrival.
func TestCopyTreeRenamesAndDropsExec(t *testing.T) {
	src := filepath.Join(t.TempDir(), "got")
	for _, p := range []string{"a.txt", ".git/config", "deep/.claude/settings.json", "deep/CLAUDE.md", "run.sh"} {
		f := filepath.Join(src, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(f), 0o755)
		if err := os.WriteFile(f, []byte(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(t.TempDir(), "ferry-1-got")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".git", "deep/.claude", "deep/CLAUDE.md"} {
		if _, err := os.Lstat(filepath.Join(dst, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s kept its name", p)
		}
	}
	for _, p := range []string{"a.txt", "dot-git/config", "deep/dot-claude/settings.json", "deep/received-CLAUDE.md", "run.sh"} {
		st, err := os.Stat(filepath.Join(dst, filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("%s: %v", p, err)
		} else if st.Mode().Perm()&0o111 != 0 {
			t.Errorf("%s is executable", p)
		}
	}
}
