// Package tests runs the wontpatch binary on the cases in tests/cases and on
// the examples, and, when an API server is available, checks every answer
// against what kubectl apply really does.
package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wontpatch-tests")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "wontpatch")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "../plugin/scripts")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building wontpatch:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	code   int
	stdout string
	stderr string
}

func runWontpatch(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running wontpatch: %v", err)
	}
	return result{code, out.String(), errb.String()}
}

type jsonFinding struct {
	Severity  string `json:"severity"`
	Code      string `json:"code"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type jsonReport struct {
	Matched  int           `json:"matched"`
	Findings []jsonFinding `json:"findings"`
}

func runJSON(t *testing.T, args ...string) (jsonReport, result) {
	t.Helper()
	r := runWontpatch(t, append([]string{"--format", "json", "--fail-on", "none"}, args...)...)
	var rep jsonReport
	if r.code != 0 {
		t.Fatalf("wontpatch %v exited %d: %s", args, r.code, r.stderr)
	}
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("wontpatch printed invalid JSON: %v\n%s", err, r.stdout)
	}
	return rep, r
}
