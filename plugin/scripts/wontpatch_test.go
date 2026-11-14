package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const deployV1 = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: web
          image: example/web:1.0
---
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
spec:
  selector:
    app: web
  ports:
    - port: 80
`

var deployV2Breaking = strings.Replace(strings.Replace(deployV1, "      app: web\n  template", "      app: web\n      tier: frontend\n  template", 1), "        app: web\n", "        app: web\n        tier: frontend\n", 1)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runArgs(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestBreakingAndCleanExitCodes(t *testing.T) {
	dir := t.TempDir()
	v1 := writeFile(t, dir, "v1.yaml", deployV1)
	v2 := writeFile(t, dir, "v2.yaml", deployV2Breaking)
	same := writeFile(t, dir, "same.yaml", strings.Replace(deployV1, "web:1.0", "web:1.1", 1))
	for _, tc := range []struct {
		new, failOn string
		want        int
	}{
		{v2, "warn", 1}, {v2, "error", 1}, {v2, "none", 0},
		{same, "warn", 0}, {same, "error", 0}, {same, "none", 0},
	} {
		code, out, errOut := runArgs(t, "", "--fail-on", tc.failOn, v1, tc.new)
		if code != tc.want {
			t.Errorf("%s --fail-on %s: exit %d, want %d\n%s%s", filepath.Base(tc.new), tc.failOn, code, tc.want, out, errOut)
		}
		if tc.new == v2 && !strings.Contains(out, "ERROR Deployment web") {
			t.Errorf("missing the selector error:\n%s", out)
		}
		if tc.new == same && !strings.Contains(out, "No immutable-field changes in the 2 objects present in both.") {
			t.Errorf("missing the clean line:\n%s", out)
		}
	}
}

func TestFlagsAfterPaths(t *testing.T) {
	dir := t.TempDir()
	v1 := writeFile(t, dir, "v1.yaml", deployV1)
	v2 := writeFile(t, dir, "v2.yaml", deployV2Breaking)
	code, out, _ := runArgs(t, "", v1, v2, "--fail-on", "none", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rep report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if rep.Errors != 1 || rep.Findings[0].Code != "selector" || rep.Findings[0].Field != "spec.selector" {
		t.Errorf("unexpected report: %+v", rep.Findings[0])
	}
}

func TestBadFlags(t *testing.T) {
	dir := t.TempDir()
	v1 := writeFile(t, dir, "v1.yaml", deployV1)
	for _, args := range [][]string{
		{"--fail-on", "sometimes", v1, v1},
		{"--format", "xml", v1, v1},
		{v1},
		{v1, v1, v1},
		{"-", "-"},
		{"--nope", v1, v1},
	} {
		code, out, _ := runArgs(t, "", args...)
		if code != exitUntrusted || out != "" {
			t.Errorf("%v: exit %d, stdout %q; want 2 and nothing on stdout", args, code, out)
		}
	}
}

func TestStdin(t *testing.T) {
	dir := t.TempDir()
	v1 := writeFile(t, dir, "v1.yaml", deployV1)
	code, out, _ := runArgs(t, deployV2Breaking, v1, "-")
	if code != 1 || !strings.Contains(out, "NEW - (2 objects)") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// garbage is input that isn't a set of manifests: what a failed render,
// a failed build or a broken pipe leaves behind.
var garbage = map[string]string{
	"empty":                    "",
	"whitespace":               "  \n\n\t\n",
	"comments only":            "# Source: chart/templates/deployment.yaml\n---\n# nothing\n",
	"separator only":           "---\n",
	"binary":                   "\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\n",
	"random text":              "the quick brown fox jumps over the lazy dog\n",
	"helm failure":             "Error: INSTALLATION FAILED: template: shop/templates/deployment.yaml:12:20: executing \"shop/templates/deployment.yaml\" at <.Values.image.tag>: nil pointer evaluating interface {}.tag\n",
	"helm error simple":        "Error: chart \"shop\" version \"9.9.9\" not found in https://charts.example.com repository\n",
	"kustomize failure":        "Error: accumulating resources: accumulation err='accumulating resources from 'base': evalsymlink failure on '/src/base' : lstat /src/base: no such file or directory'\n",
	"kubectl error":            "error: the server doesn't have a resource type \"deploymentz\"\n",
	"go build failure":         "# github.com/example/shop\n./main.go:12:2: undefined: foo\n",
	"helm dry run":             "NAME: shop\nLAST DEPLOYED: Sun Nov  8 00:00:00 2026\nNAMESPACE: shop\nSTATUS: pending-install\nREVISION: 1\nTEST SUITE: None\n",
	"values file":              "replicaCount: 2\nimage:\n  repository: example/web\n  tag: \"1.0\"\n",
	"bad yaml":                 "apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n   namespace: shop\n spec: [\n",
	"list of strings":          "- one\n- two\n",
	"no newline":               strings.TrimSuffix(deployV1, "\n"),
	"truncated json":           `{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "web"`,
	"json then garbage":        `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a"}} trailing`,
	"json array":               `["a", "b"]`,
	"no name":                  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  labels: {a: b}\n",
	"no metadata":              "apiVersion: v1\nkind: ConfigMap\ndata: {a: b}\n",
	"deployment no spec":       "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: shop\n",
	"deployment no containers": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: shop\nspec:\n  selector:\n    matchLabels: {app: web}\n  template:\n    metadata:\n      labels: {app: web}\n",
	"duplicate":                deployV1 + "---\n" + deployV1,
	"live without annotation":  "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: shop\n  uid: 1b2c\n  resourceVersion: \"42\"\nspec:\n  selector:\n    matchLabels: {app: web}\n  template:\n    metadata:\n      labels: {app: web}\n    spec:\n      containers: [{name: web, image: example/web:1.0}]\nstatus:\n  replicas: 1\n",
	"unrendered template":      "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: \"{{ include \\\"shop.fullname\\\" . }}\"\ndata:\n  a: b\n",
	"helm template file":       "{{- if .Values.enabled }}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n{{- end }}\n",
	"kustomization":            "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n",
	"content on separator":     "--- {\"apiVersion\": \"v1\", \"kind\": \"ConfigMap\", \"metadata\": {\"name\": \"a\"}}\n---\n" + deployV1,
	"api status error":         "apiVersion: v1\nkind: Status\nmetadata:\n  name: web\nstatus: Failure\nmessage: deployments.apps \"web\" not found\nreason: NotFound\ncode: 404\n",
	"list with bad item":       "apiVersion: v1\nkind: List\nitems:\n  - just a string\n",
}

// TestUntrustedInput feeds each kind of garbage as OLD and as NEW, with the
// other side real, under every --fail-on and --format, and checks that
// wontpatch exits 2 and prints no result.
func TestUntrustedInput(t *testing.T) {
	dir := t.TempDir()
	real := writeFile(t, dir, "real.yaml", deployV1)
	for name, content := range garbage {
		ext := ".yaml"
		if strings.HasPrefix(strings.TrimSpace(content), "{") || strings.HasPrefix(strings.TrimSpace(content), "[") {
			ext = ".json"
		}
		bad := writeFile(t, dir, strings.ReplaceAll(name, " ", "-")+ext, content)
		for _, order := range [][2]string{{bad, real}, {real, bad}} {
			for _, failOn := range []string{"warn", "error", "none"} {
				for _, format := range []string{"text", "json"} {
					code, out, errOut := runArgs(t, "", "--fail-on", failOn, "--format", format, order[0], order[1])
					side := "NEW"
					if order[0] == bad {
						side = "OLD"
					}
					if code != exitUntrusted {
						t.Errorf("%s as %s, --fail-on %s --format %s: exit %d, want 2\n%s", name, side, failOn, format, code, out)
					}
					if out != "" {
						t.Errorf("%s as %s: printed a result:\n%s", name, side, out)
					}
					if !strings.Contains(errOut, "can't be trusted") || !strings.Contains(errOut, side+": ") {
						t.Errorf("%s as %s: stderr doesn't say which input can't be trusted:\n%s", name, side, errOut)
					}
				}
			}
		}
	}
}

func TestUntrustedPaths(t *testing.T) {
	dir := t.TempDir()
	real := writeFile(t, dir, "real.yaml", deployV1)
	empty := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	onlyText := filepath.Join(dir, "textdir")
	writeFile(t, onlyText, "README.md", "# not a manifest\n")
	emptyFiles := filepath.Join(dir, "emptyfiles")
	writeFile(t, emptyFiles, "a.yaml", "")
	writeFile(t, emptyFiles, "b.yaml", "---\n")
	chart := filepath.Join(dir, "chart")
	writeFile(t, chart, "Chart.yaml", "apiVersion: v2\nname: shop\nversion: 1.0.0\n")
	writeFile(t, chart, "templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shop\n")
	for _, p := range []string{filepath.Join(dir, "missing.yaml"), empty, onlyText, emptyFiles, chart} {
		for _, args := range [][]string{{p, real}, {real, p}} {
			code, out, _ := runArgs(t, "", "--fail-on", "none", args[0], args[1])
			if code != exitUntrusted || out != "" {
				t.Errorf("%v: exit %d, stdout %q", args, code, out)
			}
		}
	}
	// In a directory, every file must hold an object: a file with only a
	// separator, null, comments or an empty List is a render that failed.
	for name, content := range map[string]string{"sep": "---\n", "null": "null\n", "comments": "# Source: x\n", "emptylist": "apiVersion: v1\nkind: List\nitems: []\n"} {
		d := filepath.Join(dir, "near-empty-"+name)
		writeFile(t, d, "a.yaml", deployV1)
		writeFile(t, d, "b.yaml", content)
		for _, args := range [][]string{{d, real}, {real, d}} {
			if code, out, _ := runArgs(t, "", "--fail-on", "none", args[0], args[1]); code != exitUntrusted || out != "" {
				t.Errorf("directory with a %s file: exit %d, stdout %q", name, code, out)
			}
		}
	}
	// An empty standard input is untrusted too.
	code, out, _ := runArgs(t, "", "--fail-on", "none", real, "-")
	if code != exitUntrusted || out != "" {
		t.Errorf("empty stdin: exit %d, stdout %q", code, out)
	}
}

// TestTruncated cuts each side at every byte, with the other side whole, for
// the same version and for two different versions, and checks that no cut
// passes as clean: each one exits 2, or exits 1 with a warning about the
// objects the cut dropped.
func TestTruncated(t *testing.T) {
	dir := t.TempDir()
	read := func(p string) string {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	v1 := read("../../examples/shop/v1/app.yaml")
	safe := read("../../examples/shop/v2-safe/app.yaml")
	boundary := 0
	for _, pair := range [][2]string{{v1, v1}, {v1, safe}} {
		for _, side := range []string{"OLD", "NEW"} {
			full, whole := pair[0], pair[1]
			if side == "NEW" {
				full, whole = pair[1], pair[0]
			}
			wholePath := writeFile(t, dir, "whole.yaml", whole)
			for cut := 1; cut < len(full); cut++ {
				part := writeFile(t, dir, "cut.yaml", full[:cut])
				args := []string{part, wholePath}
				if side == "NEW" {
					args = []string{wholePath, part}
				}
				code, out, _ := runArgs(t, "", args...)
				if code == exitUntrusted {
					if out != "" {
						t.Fatalf("exit 2 but printed a result")
					}
					continue
				}
				if side == "NEW" {
					if code != exitFindings || !strings.Contains(out, "WARN") {
						t.Fatalf("NEW cut at byte %d: exit %d\n%s", cut, code, out)
					}
					continue
				}
				if code != exitFindings || !strings.Contains(out, "WARN") {
					t.Fatalf("OLD cut at byte %d: exit %d\n%s", cut, code, out)
				}
				if strings.HasPrefix(full[cut:], "---\n") {
					boundary++
				}
			}
		}
	}
	t.Logf("%d cuts fell exactly between documents and exited 1 with a warning; the rest exited 2 or 1", boundary)
}

// TestCutWithDifferentLayout cuts NEW's last object on each line break when
// NEW also changed lines above the cut, so its layout differs from OLD's.
func TestCutWithDifferentLayout(t *testing.T) {
	dir := t.TempDir()
	cfg := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\ndata:\n  a: \"1\"\n---\n"
	deploy := func(extra, image string) string {
		return cfg + "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 2\n" + extra +
			"  selector:\n    matchLabels:\n      app: web\n  template:\n    metadata:\n      labels:\n        app: web\n    spec:\n      containers:\n        - name: web\n          image: " + image +
			"\n          ports:\n            - containerPort: 8080\n          resources:\n            limits:\n              memory: 512Mi\n"
	}
	old := writeFile(t, dir, "old.yaml", deploy("", "web:1.4.0"))
	full := deploy("  revisionHistoryLimit: 5\n", "web:1.5.0")
	lines := strings.SplitAfter(full, "\n")
	start := strings.Count(cfg, "\n") + 10
	for n := start; n < len(lines)-1; n++ {
		cut := writeFile(t, dir, "new.yaml", strings.Join(lines[:n], ""))
		for _, args := range [][]string{{old, cut}, {cut, old}} {
			code, out, _ := runArgs(t, "", args[0], args[1])
			if code == exitOK || strings.Contains(out, "No immutable-field changes") {
				t.Errorf("cut after line %d (%q) passed as clean:\n%s", n, strings.TrimSpace(lines[n-1]), out)
			}
		}
	}
	whole := writeFile(t, dir, "whole.yaml", full)
	if code, out, _ := runArgs(t, "", old, whole); code != exitOK {
		t.Errorf("the whole file: exit %d\n%s", code, out)
	}
}

// kubectl create --dry-run=client prints status: {loadBalancer: {}}, which
// isn't a live object.
func TestDryRunStatusIsNotLive(t *testing.T) {
	dir := t.TempDir()
	svc := "apiVersion: v1\nkind: Service\nmetadata:\n  creationTimestamp: null\n  name: web\nspec:\n  ports:\n  - port: 80\n  selector:\n    app: web\nstatus:\n  loadBalancer: {}\n"
	o := writeFile(t, dir, "old.yaml", svc)
	n := writeFile(t, dir, "new.yaml", svc)
	if code, out, errOut := runArgs(t, "", o, n); code != exitOK {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestTruncatedJSON(t *testing.T) {
	dir := t.TempDir()
	full := `{"apiVersion": "v1", "kind": "List", "items": [{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a", "namespace": "x"}, "immutable": true, "data": {"k": "v"}}]}`
	real := writeFile(t, dir, "real.json", full)
	for cut := 1; cut < len(full); cut++ {
		part := writeFile(t, dir, "cut.json", full[:cut])
		for _, args := range [][]string{{part, real}, {real, part}} {
			if code, out, _ := runArgs(t, "", "--fail-on", "none", args[0], args[1]); code != exitUntrusted {
				t.Fatalf("JSON cut at %d: exit %d\n%s", cut, code, out)
			}
		}
	}
}

func TestRemovedAndAdded(t *testing.T) {
	dir := t.TempDir()
	parts := strings.SplitN(deployV1, "---\n", 2)
	both := writeFile(t, dir, "both.yaml", deployV1)
	onlyDeploy := writeFile(t, dir, "deploy.yaml", parts[0])
	code, out, _ := runArgs(t, "", both, onlyDeploy)
	if code != 1 || !strings.Contains(out, "WARN  Service web") || !strings.Contains(out, "1 only in OLD") {
		t.Errorf("removed: exit %d\n%s", code, out)
	}
	code, out, _ = runArgs(t, "", "--fail-on", "error", both, onlyDeploy)
	if code != 0 || !strings.Contains(out, "WARN") {
		t.Errorf("removed with --fail-on error: exit %d\n%s", code, out)
	}
	// Added at the end of the file: what a cut OLD would look like too.
	code, out, _ = runArgs(t, "", onlyDeploy, both)
	if code != 1 || !strings.Contains(out, "everything after it in NEW is new: Service shop/web") {
		t.Errorf("added at the end: exit %d\n%s", code, out)
	}
	// Added before the end: created, listed in a note, and nothing to warn about.
	onlyService := writeFile(t, dir, "service.yaml", parts[1])
	code, out, _ = runArgs(t, "", onlyService, both)
	if code != 0 || !strings.Contains(out, "only in NEW, so they are created and there is nothing to compare: Deployment shop/web") || strings.Contains(out, "WARN") {
		t.Errorf("added in the middle: exit %d\n%s", code, out)
	}
}

func TestNamespaceFlag(t *testing.T) {
	dir := t.TempDir()
	noNS := strings.ReplaceAll(deployV1, "  namespace: shop\n", "")
	old := writeFile(t, dir, "old.yaml", noNS)
	nw := writeFile(t, dir, "new.yaml", deployV2Breaking)
	_, out, _ := runArgs(t, "", "--fail-on", "none", old, nw)
	if !strings.Contains(out, "0 matched") {
		t.Errorf("without --namespace the objects shouldn't match:\n%s", out)
	}
	code, out, _ := runArgs(t, "", "--namespace", "shop", old, nw)
	if code != 1 || !strings.Contains(out, "2 matched") || !strings.Contains(out, "ERROR Deployment web") {
		t.Errorf("with --namespace: exit %d\n%s", code, out)
	}
}

func jobYAML(image, extraMeta, extraSpec string) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: migrate
  namespace: shop
%sspec:
%s  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: %s
`, extraMeta, extraSpec, image)
}

func TestJobRules(t *testing.T) {
	dir := t.TempDir()
	hook := "  annotations:\n    helm.sh/hook: pre-upgrade\n"
	for _, tc := range []struct {
		name     string
		old, new string
		want     string // severity of the job finding, or "" for none
		wantExit int
	}{
		{"image change", jobYAML("m:1", "", ""), jobYAML("m:2", "", ""), "ERROR", 1},
		{"same", jobYAML("m:1", "", ""), jobYAML("m:1", "", "  backoffLimit: 3\n"), "", 0},
		{"helm hook", jobYAML("m:1", hook, ""), jobYAML("m:2", hook, ""), "note", 0},
		{"ttl", jobYAML("m:1", "", "  ttlSecondsAfterFinished: 300\n"), jobYAML("m:2", "", "  ttlSecondsAfterFinished: 300\n"), "WARN", 1},
		{"argo hook", jobYAML("m:1", "  annotations:\n    argocd.argoproj.io/hook: PreSync\n", ""), jobYAML("m:2", "  annotations:\n    argocd.argoproj.io/hook: PreSync\n", ""), "note", 0},
	} {
		o := writeFile(t, dir, tc.name+"/old.yaml", tc.old)
		n := writeFile(t, dir, tc.name+"/new.yaml", tc.new)
		code, out, errOut := runArgs(t, "", "--format", "json", o, n)
		if code != tc.wantExit {
			t.Errorf("%s: exit %d, want %d\n%s%s", tc.name, code, tc.wantExit, out, errOut)
			continue
		}
		var rep report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, f := range rep.Findings {
			if f.Kind == "Job" {
				got = f.Severity
			}
		}
		if got != tc.want {
			t.Errorf("%s: job finding %q, want %q\n%s", tc.name, got, tc.want, out)
		}
	}
}

func TestGenerateNameSkipped(t *testing.T) {
	dir := t.TempDir()
	gen := "apiVersion: batch/v1\nkind: Job\nmetadata:\n  generateName: migrate-\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers: [{name: m, image: m:1}]\n"
	o := writeFile(t, dir, "old.yaml", deployV1+"---\n"+gen)
	n := writeFile(t, dir, "new.yaml", deployV1+"---\n"+strings.Replace(gen, "m:1", "m:2", 1))
	code, out, _ := runArgs(t, "", o, n)
	if code != 0 || !strings.Contains(out, "generateName") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A live object exported with kubectl get -o yaml is compared through its
// last-applied-configuration annotation.
func TestLiveListWithLastApplied(t *testing.T) {
	dir := t.TempDir()
	applied := map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "rb", "namespace": "shop"},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "reader"},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "shop", "namespace": "shop"}},
	}
	b, _ := json.Marshal(applied)
	live := fmt.Sprintf(`apiVersion: v1
kind: List
items:
  - apiVersion: rbac.authorization.k8s.io/v1
    kind: RoleBinding
    metadata:
      name: rb
      namespace: shop
      uid: 0f6c
      resourceVersion: "123"
      creationTimestamp: "2026-11-01T00:00:00Z"
      annotations:
        kubectl.kubernetes.io/last-applied-configuration: %q
    roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: reader}
    subjects: [{kind: ServiceAccount, name: shop, namespace: shop}]
`, string(b))
	o := writeFile(t, dir, "live.yaml", live)
	changed := strings.Replace(string(b), `"name":"reader"`, `"name":"admin"`, 1)
	n := writeFile(t, dir, "new.json", changed)
	code, out, errOut := runArgs(t, "", o, n)
	if code != 1 || !strings.Contains(out, "ERROR RoleBinding rb") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	same := writeFile(t, dir, "same.json", string(b))
	code, out, _ = runArgs(t, "", o, same)
	if code != 0 {
		t.Errorf("same: exit %d\n%s", code, out)
	}
	// Live objects nobody applied, like kube-root-ca.crt, are left out when
	// NEW doesn't have them, and refused when it does.
	rootCA := "  - apiVersion: v1\n    kind: ConfigMap\n    metadata:\n      name: kube-root-ca.crt\n      namespace: shop\n      uid: 77aa\n      resourceVersion: \"9\"\n    data:\n      ca.crt: x\n"
	withCA := writeFile(t, dir, "live-ca.yaml", live+rootCA)
	code, out, errOut = runArgs(t, "", withCA, same)
	if code != 0 || !strings.Contains(out, "kube-root-ca.crt was read from a cluster without a last-applied annotation and isn't in NEW") || strings.Contains(out, "only in OLD") && !strings.Contains(out, "0 only in OLD") {
		t.Errorf("live object not in NEW: exit %d\n%s%s", code, out, errOut)
	}
	caNew := writeFile(t, dir, "ca-new.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kube-root-ca.crt\n  namespace: shop\ndata:\n  ca.crt: y\n")
	if code, out, _ := runArgs(t, "", "--fail-on", "none", withCA, caNew); code != exitUntrusted || out != "" {
		t.Errorf("live object without annotation that NEW has: exit %d\n%s", code, out)
	}
	// An annotation that doesn't parse, or describes another object, is untrusted.
	for _, bad := range []string{"{not json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"other"}}`} {
		o := writeFile(t, dir, "badlive.yaml", strings.Replace(live, fmt.Sprintf("%q", string(b)), fmt.Sprintf("%q", bad), 1))
		if code, out, _ := runArgs(t, "", "--fail-on", "none", o, same); code != exitUntrusted {
			t.Errorf("bad annotation %q: exit %d\n%s", bad, code, out)
		}
	}
}

func TestSecretValuesNotPrinted(t *testing.T) {
	dir := t.TempDir()
	s := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\n  namespace: shop\nimmutable: true\nstringData:\n  password: %s\n"
	o := writeFile(t, dir, "old.yaml", fmt.Sprintf(s, "first-secret-value"))
	n := writeFile(t, dir, "new.yaml", fmt.Sprintf(s, "second-secret-value"))
	for _, format := range []string{"text", "json"} {
		code, out, _ := runArgs(t, "", "--format", format, o, n)
		if code != 1 || !strings.Contains(out, "data.password changed") {
			t.Errorf("%s: exit %d\n%s", format, code, out)
		}
		for _, secret := range []string{"first-secret-value", "second-secret-value", "Zmlyc3Qtc2VjcmV0LXZhbHVl", "c2Vjb25kLXNlY3JldC12YWx1ZQ=="} {
			if strings.Contains(out, secret) {
				t.Errorf("%s output contains a secret value:\n%s", format, out)
			}
		}
	}
}

func TestParseQuantity(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{
		{"1Gi", "1024Mi", true}, {"1G", "1000M", true}, {"500m", "0.5", true}, {"1e3", "1k", true},
		{"128Mi", "134217728", true}, {"1Gi", "1G", false}, {"10Gi", "20Gi", false},
	} {
		x, ok1 := parseQuantity(tc.a)
		y, ok2 := parseQuantity(tc.b)
		if !ok1 || !ok2 {
			t.Fatalf("couldn't parse %s or %s", tc.a, tc.b)
		}
		if (x.Cmp(y) == 0) != tc.equal {
			t.Errorf("%s == %s: got %v", tc.a, tc.b, !tc.equal)
		}
	}
	for _, bad := range []string{"", "Gi", "1Zi", "one", "1e99"} {
		if _, ok := parseQuantity(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestDefaultPullPolicy(t *testing.T) {
	for image, want := range map[string]string{
		"nginx": "Always", "nginx:latest": "Always", "nginx:1.27": "IfNotPresent",
		"registry:5000/app": "Always", "registry:5000/app:2": "IfNotPresent",
		"nginx@sha256:abc": "IfNotPresent",
		"busybox:latest@sha256:2919d0172f7524b2d8df9e50066a682669e6d170ac0f6a49676d54358fe970b5": "Always",
		"busybox@sha256:2919d0172f7524b2d8df9e50066a682669e6d170ac0f6a49676d54358fe970b5":        "IfNotPresent",
		"Busybox": "IfNotPresent", "localhost:5000/app": "Always", "Registry.Example.com/app:1": "IfNotPresent",
	} {
		if got := defaultPullPolicy(image); got != want {
			t.Errorf("%s: %s, want %s", image, got, want)
		}
	}
}

func TestSplitYAML(t *testing.T) {
	docs := splitYAML([]byte("a: 1\n--- # comment\nb: 2\n---\nc: |\n  ---not a separator\n----\n"))
	if len(docs) != 3 {
		t.Fatalf("got %d documents", len(docs))
	}
	if docs[1].line != 3 || docs[2].line != 5 {
		t.Errorf("lines %d, %d", docs[1].line, docs[2].line)
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := runArgs(t, "", "--version")
	if code != 0 || strings.TrimSpace(out) != "wontpatch "+version {
		t.Errorf("exit %d, %q", code, out)
	}
}

// normPodTemplate fills in the API-server defaults so a manifest that leaves
// them out compares equal to one that spells them out. The in-tree volume
// sources were missing, which made a pasted `kubectl get -o yaml` look like a
// change.
func TestNormPodTemplateFillsInTreeVolumeDefaults(t *testing.T) {
	in := map[string]any{
		"spec": map[string]any{
			"volumes": []any{
				map[string]any{"name": "rbd", "rbd": map[string]any{"image": "pool/image"}},
				map[string]any{"name": "sio", "scaleIO": map[string]any{"gateway": "https://sio", "system": "sio"}},
				map[string]any{"name": "disk", "azureDisk": map[string]any{"diskName": "d", "diskURI": "https://d"}},
			},
		},
	}

	out, ok := normPodTemplate(in).(map[string]any)
	if !ok {
		t.Fatal("normPodTemplate did not return a map")
	}
	vols, _ := out["spec"].(map[string]any)["volumes"].([]any)
	byName := map[string]map[string]any{}
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		name, _ := vm["name"].(string)
		byName[name] = vm
	}

	for _, tc := range []struct{ volume, source, field, want string }{
		{"rbd", "rbd", "pool", "rbd"},
		{"rbd", "rbd", "user", "admin"},
		{"rbd", "rbd", "keyring", "/etc/ceph/keyring"},
		{"sio", "scaleIO", "fsType", "xfs"},
		{"sio", "scaleIO", "storageMode", "ThinProvisioned"},
		{"disk", "azureDisk", "cachingMode", "ReadWrite"},
		{"disk", "azureDisk", "fsType", "ext4"},
		{"disk", "azureDisk", "kind", "Shared"},
	} {
		vol, ok := byName[tc.volume]
		if !ok {
			t.Fatalf("volume %s missing from the normalized template", tc.volume)
		}
		src, ok := vol[tc.source].(map[string]any)
		if !ok {
			t.Fatalf("volume %s has no %s source", tc.volume, tc.source)
		}
		if got := src[tc.field]; got != tc.want {
			t.Errorf("%s.%s.%s = %v, want %v", tc.volume, tc.source, tc.field, got, tc.want)
		}
	}
}

// A value the user set must survive the normalization.
func TestNormPodTemplateKeepsExplicitVolumeDefaults(t *testing.T) {
	in := map[string]any{
		"spec": map[string]any{
			"volumes": []any{
				map[string]any{"name": "rbd", "rbd": map[string]any{
					"image": "pool/image", "pool": "custom-pool", "user": "ceph", "keyring": "/other/keyring",
				}},
				map[string]any{"name": "disk", "azureDisk": map[string]any{
					"diskName": "d", "diskURI": "https://d", "kind": "Managed", "fsType": "xfs",
				}},
			},
		},
	}

	out, _ := normPodTemplate(in).(map[string]any)
	vols, _ := out["spec"].(map[string]any)["volumes"].([]any)
	byName := map[string]map[string]any{}
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		name, _ := vm["name"].(string)
		byName[name] = vm
	}

	rbd, _ := byName["rbd"]["rbd"].(map[string]any)
	for field, want := range map[string]string{"pool": "custom-pool", "user": "ceph", "keyring": "/other/keyring"} {
		if got := rbd[field]; got != want {
			t.Errorf("rbd.%s overwritten: got %v, want %v", field, got, want)
		}
	}
	disk, _ := byName["disk"]["azureDisk"].(map[string]any)
	for field, want := range map[string]string{"kind": "Managed", "fsType": "xfs", "cachingMode": "ReadWrite"} {
		if got := disk[field]; got != want {
			t.Errorf("azureDisk.%s = %v, want %v", field, got, want)
		}
	}
}
