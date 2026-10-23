package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func writeAuditFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func collectedAuditSources(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	forEachSourceFile(t, root, func(rel string, _ []byte) { paths = append(paths, rel) })
	sort.Strings(paths)
	return paths
}

func TestForEachSourceFile_GitSourceBoundary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git fixture requires git; source archives use the filesystem fallback")
	}
	for _, separateGitDir := range []bool{false, true} {
		name := "git-directory"
		if separateGitDir {
			name = "git-pointer-file"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) {
				t.Helper()
				command := exec.Command("git", append([]string{"-C", root}, args...)...)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
			}
			if separateGitDir {
				git("init", "--quiet", "--separate-git-dir", filepath.Join(t.TempDir(), "metadata"))
			} else {
				git("init", "--quiet")
			}
			marker, err := os.Stat(filepath.Join(root, ".git"))
			if err != nil || marker.IsDir() == separateGitDir {
				t.Fatalf("unexpected Git metadata marker: %v", err)
			}
			writeAuditFixture(t, root, ".gitignore", "copies/\nignored.go\ntracked.go\n")
			for _, rel := range []string{
				"tracked.go", "deleted.go", "new.go", "internal/new.go", "ignored.go",
				"copies/backend/internal/copied.go", "vendor/dependency.go", "node_modules/tool.go",
				"testdata/fixture.go", ".cache/generated.go", "internal/sample_test.go",
			} {
				writeAuditFixture(t, root, rel, "package fixture\n")
			}
			writeAuditFixture(t, root, "copies/backend/.git", "gitdir: /unused/worktree\n")
			git("add", "--force", "tracked.go", "deleted.go")
			if err := os.Remove(filepath.Join(root, "deleted.go")); err != nil {
				t.Fatal(err)
			}
			want := []string{"internal/new.go", "new.go", "tracked.go"}
			if got := collectedAuditSources(t, root); !reflect.DeepEqual(got, want) {
				t.Fatalf("audited sources = %v, want %v; tracked ignored files and new source must remain protected", got, want)
			}
		})
	}
}

func TestForEachSourceFile_ArchiveExcludesNestedGitWorktree(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".archive")
	writeAuditFixture(t, root, "internal/source.go", "package source\n")
	writeAuditFixture(t, root, "copies/backend/.git", "gitdir: /unused/worktree\n")
	writeAuditFixture(t, root, "copies/backend/internal/copied.go", "package copied\n")
	writeAuditFixture(t, root, "clone/.git/config", "[core]\n")
	writeAuditFixture(t, root, "clone/source.go", "package copied\n")
	want := []string{"internal/source.go"}
	if got := collectedAuditSources(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("archive source scan crossed a nested repository boundary: %v", got)
	}
}
