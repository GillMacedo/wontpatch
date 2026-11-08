package tests

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Each directory in tests/cases holds old.json, new.json and want, which is
// one of:
//
//	error: wontpatch reports an ERROR, and the API server rejects the apply
//	ok:    wontpatch reports no ERROR or WARN, and the API server accepts it
//	warn:  wontpatch reports a WARN and no ERROR; the outcome depends on the cluster
//
// {{name}} (in JSON cases) and @R@ (in YAML cases) stand for the name of a
// cluster-scoped object and {{ipN}} for a
// Service cluster IP, so each run against a real API server can use fresh ones.
type testCase struct {
	name     string
	old, new string
	ext      string
	want     string
}

func loadCases(t *testing.T) []testCase {
	t.Helper()
	dirs, err := filepath.Glob("cases/*")
	if err != nil {
		t.Fatal(err)
	}
	var out []testCase
	for _, d := range dirs {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			continue
		}
		want, err := os.ReadFile(filepath.Join(d, "want"))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		ext := ".json"
		if _, err := os.Stat(filepath.Join(d, "old.yaml")); err == nil {
			ext = ".yaml"
		}
		old, err1 := os.ReadFile(filepath.Join(d, "old"+ext))
		nw, err2 := os.ReadFile(filepath.Join(d, "new"+ext))
		if err1 != nil || err2 != nil {
			t.Fatalf("%s: missing old and new manifests", d)
		}
		w := strings.TrimSpace(string(want))
		if w != "error" && w != "ok" && w != "warn" {
			t.Fatalf("%s: want is %q", d, w)
		}
		out = append(out, testCase{filepath.Base(d), string(old), string(nw), ext, w})
	}
	if len(out) < 100 {
		t.Fatalf("only %d cases found", len(out))
	}
	return out
}

func fill(s string, names map[string]string) string {
	for k, v := range names {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

func writeCase(t *testing.T, c testCase, names map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	o, n := filepath.Join(dir, "old"+c.ext), filepath.Join(dir, "new"+c.ext)
	if err := os.WriteFile(o, []byte(fill(c.old, names)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n, []byte(fill(c.new, names)), 0o644); err != nil {
		t.Fatal(err)
	}
	return o, n
}

func severities(rep jsonReport) map[string]bool {
	s := map[string]bool{}
	for _, f := range rep.Findings {
		s[f.Severity] = true
	}
	return s
}

// TestCases checks wontpatch's answer for every case, with no cluster.
func TestCases(t *testing.T) {
	names := map[string]string{"{{name}}": "case-object", "@R@": "case", "{{ip1}}": "10.0.1.1", "{{ip2}}": "10.0.1.2", "{{ip3}}": "10.0.1.3"}
	counts := map[string]int{}
	for _, c := range loadCases(t) {
		c := c
		t.Run(c.name, func(t *testing.T) {
			o, n := writeCase(t, c, names)
			rep, r := runJSON(t, o, n)
			sev := severities(rep)
			switch c.want {
			case "error":
				if !sev["ERROR"] {
					t.Errorf("want an ERROR, got %s", r.stdout)
				}
			case "warn":
				if sev["ERROR"] || !sev["WARN"] {
					t.Errorf("want a WARN and no ERROR, got %s", r.stdout)
				}
			case "ok":
				if sev["ERROR"] || sev["WARN"] {
					t.Errorf("want no ERROR or WARN, got %s", r.stdout)
				}
			}
			if rep.Matched < 1 {
				t.Errorf("matched %d objects", rep.Matched)
			}
		})
		counts[c.want]++
	}
	t.Logf("cases: %d error, %d ok, %d warn", counts["error"], counts["ok"], counts["warn"])
}

// kubectl returns the kubectl to use for ground-truth tests, or skips.
func kubectl(t *testing.T) string {
	t.Helper()
	k := os.Getenv("WONTPATCH_KUBECTL")
	if k == "" || os.Getenv("KUBECONFIG") == "" {
		t.Skip("set WONTPATCH_KUBECTL and KUBECONFIG to check answers against an API server (see scripts/apiserver.sh)")
	}
	return k
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()%1e9, rand.Intn(1e6))
}

// TestGroundTruth applies each case's old manifest and then its new one to a
// real API server, with client-side and with server-side apply, and checks
// that the server agrees with want.
func TestGroundTruth(t *testing.T) {
	k := kubectl(t)
	disagree := 0
	var log []string
	for _, c := range loadCases(t) {
		for _, mode := range []string{"client", "server"} {
			c, mode := c, mode
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				ns := uniqueName("gt")
				names := map[string]string{"{{name}}": ns, "@R@": ns}
				for i := 1; i <= 3; i++ {
					names[fmt.Sprintf("{{ip%d}}", i)] = fmt.Sprintf("10.0.%d.%d", 2+rand.Intn(250), 2+rand.Intn(250))
				}
				o, n := writeCase(t, c, names)
				if out, err := exec.Command(k, "create", "namespace", ns).CombinedOutput(); err != nil {
					t.Fatalf("create namespace: %v: %s", err, out)
				}
				oldArgs := []string{"-n", ns, "apply", "-f", o}
				if mode == "server" {
					oldArgs = append(oldArgs, "--server-side")
				}
				if out, err := exec.Command(k, oldArgs...).CombinedOutput(); err != nil {
					t.Fatalf("applying old.json failed, so the case is broken: %s", out)
				}
				args := []string{"-n", ns, "apply", "-f", n}
				if mode == "server" {
					args = append(args, "--server-side", "--force-conflicts")
				}
				out, err := exec.Command(k, args...).CombinedOutput()
				accepted := err == nil
				switch {
				case c.want == "error" && accepted:
					disagree++
					t.Errorf("wontpatch says the API server rejects this, but it was accepted: %s", out)
				case c.want == "ok" && !accepted:
					disagree++
					t.Errorf("wontpatch says this is fine, but the API server rejected it: %s", out)
				case c.want == "warn":
					log = append(log, fmt.Sprintf("%s/%s accepted=%v", c.name, mode, accepted))
				}
			})
		}
	}
	sort.Strings(log)
	t.Logf("warn cases on this API server: %s", strings.Join(log, ", "))
	t.Logf("disagreements: %d", disagree)
}
