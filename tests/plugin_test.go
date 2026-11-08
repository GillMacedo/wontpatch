package tests

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInstalledPlugin copies plugin/ to a directory outside this module, the
// way Claude Code installs it, and builds it with the command the skill uses.
// The plugin has no go.mod, so it must build from its .go files alone.
func TestInstalledPlugin(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "wontpatch")
	err := filepath.WalkDir("../plugin", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("../plugin", p)
		target := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile("../plugin/skills/checking-immutable-fields/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	const buildLine = `go build -o "${TMPDIR:-/tmp}/wontpatch" $(ls "${CLAUDE_PLUGIN_ROOT}"/scripts/*.go | grep -v _test.go)`
	if !strings.Contains(string(skill), buildLine) {
		t.Fatalf("SKILL.md doesn't contain the build line this test runs:\n%s", buildLine)
	}
	if runtime.GOOS == "windows" {
		t.Skip("the skill's build line is for a POSIX shell")
	}
	out := filepath.Join(dir, "tmp")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", buildLine)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+root, "TMPDIR="+out, "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the installed plugin failed: %v\n%s", err, b)
	}
	abs, _ := filepath.Abs("../examples/shop")
	run := exec.Command(filepath.Join(out, "wontpatch"), filepath.Join(abs, "v1"), filepath.Join(abs, "v2-breaking"))
	b, err := run.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 || !strings.Contains(string(b), "6 errors, 1 warning.") {
		t.Fatalf("the installed plugin's binary gave %v:\n%s", err, b)
	}
}
