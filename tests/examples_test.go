package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var wantBreakingErrors = []string{
	"ConfigMap shop-config", "Deployment web", "Job migrate", "RoleBinding shop-reader", "Service web", "StatefulSet db",
}
var wantBreakingWarnings = []string{"PersistentVolumeClaim uploads"}

func objectsWith(rep jsonReport, sev string) []string {
	var out []string
	for _, f := range rep.Findings {
		if f.Severity == sev && f.Kind != "" {
			out = append(out, f.Kind+" "+f.Name)
		}
	}
	sort.Strings(out)
	return out
}

func TestBreakingExample(t *testing.T) {
	rep, _ := runJSON(t, "../examples/shop/v1", "../examples/shop/v2-breaking")
	if got := objectsWith(rep, "ERROR"); !reflect.DeepEqual(got, wantBreakingErrors) {
		t.Errorf("errors on %v, want %v", got, wantBreakingErrors)
	}
	if got := objectsWith(rep, "WARN"); !reflect.DeepEqual(got, wantBreakingWarnings) {
		t.Errorf("warnings on %v, want %v", got, wantBreakingWarnings)
	}
	r := runWontpatch(t, "../examples/shop/v1", "../examples/shop/v2-breaking")
	if r.code != 1 {
		t.Errorf("exit %d, want 1", r.code)
	}
	if strings.Contains(r.stdout, "No immutable-field changes") {
		t.Errorf("printed a clean result for the breaking example")
	}
}

func TestSafeExample(t *testing.T) {
	r := runWontpatch(t, "../examples/shop/v1", "../examples/shop/v2-safe")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0: %s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "No immutable-field changes in the 9 objects present in both.") {
		t.Errorf("unexpected output:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "ERROR") || strings.Contains(r.stdout, "WARN") {
		t.Errorf("the safe example has findings:\n%s", r.stdout)
	}
}

// splitDocs splits the example file into its documents.
func splitDocs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var docs []string
	for _, d := range strings.Split(string(data), "\n---\n") {
		if strings.Contains(d, "kind:") {
			docs = append(docs, d+"\n")
		}
	}
	return docs
}

func docName(doc string) string {
	kind, name := "", ""
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "kind: ") {
			kind = strings.TrimPrefix(l, "kind: ")
		}
		if strings.HasPrefix(l, "  name: ") && name == "" {
			name = strings.TrimPrefix(l, "  name: ")
		}
	}
	return kind + " " + name
}

// applyExample applies v1, then each object of next on its own, and returns
// the objects the API server rejected.
func applyExample(t *testing.T, k, next string) []string {
	t.Helper()
	ns := uniqueName("shop")
	dir := t.TempDir()
	write := func(name, doc string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.ReplaceAll(doc, "namespace: shop", "namespace: "+ns)), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if out, err := exec.Command(k, "create", "namespace", ns).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	v1, err := os.ReadFile("../examples/shop/v1/app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(k, "apply", "-f", write("v1.yaml", string(v1))).CombinedOutput(); err != nil {
		t.Fatalf("applying v1: %v: %s", err, out)
	}
	var rejected []string
	for i, doc := range splitDocs(t, "../examples/shop/"+next+"/app.yaml") {
		p := write("doc"+string(rune('a'+i))+".yaml", doc)
		if out, err := exec.Command(k, "apply", "-f", p).CombinedOutput(); err != nil {
			t.Logf("%s rejected: %s", docName(doc), firstLineOf(string(out)))
			rejected = append(rejected, docName(doc))
		}
	}
	sort.Strings(rejected)
	return rejected
}

func firstLineOf(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "invalid") || strings.Contains(l, "forbidden") || strings.Contains(l, "Invalid") {
			if len(l) > 200 {
				l = l[:200]
			}
			return l
		}
	}
	return strings.SplitN(s, "\n", 2)[0]
}

// TestExamplesGroundTruth checks that the objects the API server rejects are
// exactly the ones wontpatch reports as errors, plus possibly the ones it
// warns about, and that the safe example applies cleanly.
func TestExamplesGroundTruth(t *testing.T) {
	k := kubectl(t)
	rejected := applyExample(t, k, "v2-breaking")
	isWarn := map[string]bool{}
	for _, w := range wantBreakingWarnings {
		isWarn[w] = true
	}
	var rejectedErrors []string
	for _, r := range rejected {
		if !isWarn[r] {
			rejectedErrors = append(rejectedErrors, r)
		}
	}
	if !reflect.DeepEqual(rejectedErrors, wantBreakingErrors) {
		t.Errorf("the API server rejected %v; wontpatch reports errors on %v", rejected, wantBreakingErrors)
	}
	if got := applyExample(t, k, "v2-safe"); len(got) != 0 {
		t.Errorf("the API server rejected %v from the safe example", got)
	}
}
