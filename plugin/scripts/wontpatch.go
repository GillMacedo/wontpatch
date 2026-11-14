// Command wontpatch compares two sets of rendered Kubernetes manifests, the
// one already applied and the one about to be applied, and reports changes
// the API server rejects because the field can't be changed after creation:
// a Deployment selector, a StatefulSet volumeClaimTemplate, a Job's pod
// template, a RoleBinding's roleRef and others. It reads files only and never
// talks to a cluster.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const version = "0.1.0"

// Exit codes.
const (
	exitOK        = 0
	exitFindings  = 1
	exitUntrusted = 2
)

// Severities, ordered.
const (
	sevNote  = 0
	sevWarn  = 1
	sevError = 2
)

var sevNames = map[int]string{sevNote: "note", sevWarn: "WARN", sevError: "ERROR"}

type object struct {
	File       string         `json:"file"`
	Line       int            `json:"line"`
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Namespace  string         `json:"namespace,omitempty"`
	Name       string         `json:"name"`
	FromLive   bool           `json:"fromLastApplied,omitempty"`
	raw        map[string]any // the manifest as applied
	text       string         // the YAML document it came from, if any
	liveOnly   bool           // read from a cluster, with no last-applied annotation
	last       bool           // it is the last document in its file
}

type finding struct {
	Severity  string `json:"severity"`
	sev       int
	Code      string   `json:"code"`
	Kind      string   `json:"kind"`
	Namespace string   `json:"namespace,omitempty"`
	Name      string   `json:"name"`
	File      string   `json:"file"`
	Line      int      `json:"line"`
	Field     string   `json:"field,omitempty"`
	Old       string   `json:"old,omitempty"`
	New       string   `json:"new,omitempty"`
	Message   string   `json:"message"`
	Details   []string `json:"details,omitempty"`
	Fix       string   `json:"fix,omitempty"`
}

// untrustedError means the input can't be read as a complete set of
// manifests, so no result may be printed.
type untrustedError struct{ msgs []string }

func (e *untrustedError) Error() string { return strings.Join(e.msgs, "; ") }

func untrusted(format string, a ...any) error {
	return &untrustedError{msgs: []string{fmt.Sprintf(format, a...)}}
}

type side struct {
	Label   string    `json:"path"`
	Files   int       `json:"files"`
	Objects []*object `json:"-"`
	Count   int       `json:"objects"`
	byKey   map[string]*object
	skipped []string
}

type report struct {
	Version  string     `json:"version"`
	Old      *side      `json:"old"`
	New      *side      `json:"new"`
	Matched  int        `json:"matched"`
	Added    int        `json:"added"`
	Removed  int        `json:"removed"`
	Findings []*finding `json:"findings"`
	Errors   int        `json:"errors"`
	Warnings int        `json:"warnings"`
	Notes    int        `json:"notes"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `Usage: wontpatch [flags] OLD NEW

OLD is what is applied now and NEW is what you are about to apply. Each is a
file or a directory of .yaml, .yml or .json files holding rendered manifests,
such as the output of helm template, helm get manifest, kustomize build, or
kubectl get -o yaml for objects applied with kubectl apply. Use - to read one
side from standard input.

Flags:
`

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("wontpatch", flag.ContinueOnError)
	fset.SetOutput(stderr)
	failOn := fset.String("fail-on", "warn", "exit 1 on findings at or above this level: error, warn or none")
	format := fset.String("format", "text", "output format: text or json")
	namespace := fset.String("namespace", "", "namespace for namespaced objects that don't set one, as helm -n or kubectl -n would")
	showVersion := fset.Bool("version", false, "print the version and exit")
	fset.Usage = func() {
		fmt.Fprint(stderr, usage)
		fset.PrintDefaults()
	}
	var pos []string
	rest := args
	for {
		if err := fset.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return exitOK
			}
			return exitUntrusted
		}
		if fset.NArg() == 0 {
			break
		}
		pos = append(pos, fset.Arg(0))
		rest = fset.Args()[1:]
	}
	if *showVersion {
		fmt.Fprintln(stdout, "wontpatch", version)
		return exitOK
	}
	threshold := map[string]int{"error": sevError, "warn": sevWarn, "none": 99}
	limit, ok := threshold[*failOn]
	if !ok {
		fmt.Fprintf(stderr, "wontpatch: --fail-on must be error, warn or none, not %q\n", *failOn)
		return exitUntrusted
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "wontpatch: --format must be text or json, not %q\n", *format)
		return exitUntrusted
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "wontpatch: need exactly two paths, OLD and NEW")
		fset.Usage()
		return exitUntrusted
	}
	if pos[0] == "-" && pos[1] == "-" {
		fmt.Fprintln(stderr, "wontpatch: only one side can be read from standard input")
		return exitUntrusted
	}

	var problems []string
	oldSide, err := loadSide(pos[0], stdin, *namespace)
	if err != nil {
		problems = append(problems, prefix("OLD", err)...)
	}
	newSide, err := loadSide(pos[1], stdin, *namespace)
	if err != nil {
		problems = append(problems, prefix("NEW", err)...)
	}
	if len(problems) > 0 {
		fmt.Fprintln(stderr, "wontpatch: the input can't be trusted, so nothing was compared and no result is given:")
		for _, p := range problems {
			fmt.Fprintln(stderr, "  "+p)
		}
		return exitUntrusted
	}

	problems = append(problems, liveWithoutAnnotation(oldSide, newSide)...)
	if len(problems) > 0 {
		fmt.Fprintln(stderr, "wontpatch: the input can't be trusted, so nothing was compared and no result is given:")
		for _, p := range problems {
			fmt.Fprintln(stderr, "  "+p)
		}
		return exitUntrusted
	}

	rep := compare(oldSide, newSide)
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "wontpatch:", err)
			return exitUntrusted
		}
	} else {
		printText(stdout, rep)
	}
	worst := -1
	for _, f := range rep.Findings {
		if f.sev > worst {
			worst = f.sev
		}
	}
	if worst >= limit {
		return exitFindings
	}
	return exitOK
}

func prefix(label string, err error) []string {
	var ue *untrustedError
	if errors.As(err, &ue) {
		out := make([]string, len(ue.msgs))
		for i, m := range ue.msgs {
			out[i] = label + ": " + m
		}
		return out
	}
	return []string{label + ": " + err.Error()}
}

// ---------- loading ----------

func loadSide(path string, stdin io.Reader, defaultNS string) (*side, error) {
	s := &side{Label: path, byKey: map[string]*object{}}
	type input struct {
		name string
		data []byte
	}
	var inputs []input
	if path == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, untrusted("reading standard input: %v", err)
		}
		inputs = append(inputs, input{"(stdin)", data})
	} else {
		info, err := os.Stat(path)
		if err != nil {
			return nil, untrusted("%v", err)
		}
		if info.IsDir() {
			err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				if d.Name() == "Chart.yaml" {
					return untrusted("%s is a Helm chart, not rendered manifests: run helm template (or helm get manifest for what is deployed) and pass its output", filepath.Dir(p))
				}
				switch strings.ToLower(filepath.Ext(p)) {
				case ".yaml", ".yml", ".json":
					data, err := os.ReadFile(p)
					if err != nil {
						return err
					}
					inputs = append(inputs, input{p, data})
				}
				return nil
			})
			if err != nil {
				var ue *untrustedError
				if errors.As(err, &ue) {
					return nil, ue
				}
				return nil, untrusted("%v", err)
			}
			if len(inputs) == 0 {
				return nil, untrusted("%s has no .yaml, .yml or .json files", path)
			}
		} else {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, untrusted("%v", err)
			}
			inputs = append(inputs, input{path, data})
		}
	}
	var msgs []string
	for _, in := range inputs {
		objs, skipped, err := parseFile(in.name, in.data)
		if err != nil {
			msgs = append(msgs, err.Error())
			continue
		}
		if len(inputs) > 1 && len(objs) == 0 && len(skipped) == 0 {
			msgs = append(msgs, fmt.Sprintf("%s has no Kubernetes objects. A file with nothing in it usually means a render that failed or was cut off", in.name))
			continue
		}
		s.skipped = append(s.skipped, skipped...)
		s.Objects = append(s.Objects, objs...)
	}
	s.Files = len(inputs)
	if len(msgs) == 0 && len(s.Objects) == 0 {
		msgs = append(msgs, fmt.Sprintf("%s has no Kubernetes objects. An empty render usually means the render failed or the wrong values were used", path))
	}
	for _, o := range s.Objects {
		if !clusterScoped[o.Kind] && o.Namespace == "" {
			o.Namespace = defaultNS
		}
		if clusterScoped[o.Kind] {
			o.Namespace = ""
		}
		if m := requiredMissing(o); m != "" {
			msgs = append(msgs, fmt.Sprintf("%s:%d: %s %s has no %s, which the API server requires, so the file may be cut off or broken", o.File, o.Line, o.Kind, o.Name, m))
			continue
		}
		k := key(o)
		if prev, ok := s.byKey[k]; ok {
			msgs = append(msgs, fmt.Sprintf("%s:%d: %s %s is defined twice (also at %s:%d), so it isn't clear which one is applied", o.File, o.Line, o.Kind, display(o), prev.File, prev.Line))
			continue
		}
		s.byKey[k] = o
	}
	if len(msgs) > 0 {
		return nil, &untrustedError{msgs: msgs}
	}
	s.Count = len(s.Objects)
	return s, nil
}

var docSep = regexp.MustCompile(`^---(\s.*)?$`)

// parseFile reads one file of YAML documents or JSON values.
func parseFile(name string, data []byte) ([]*object, []string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("%s is empty", name)
	}
	if !utf8Text(data) {
		return nil, nil, fmt.Errorf("%s is not text", name)
	}
	var docs []docText
	if trimmed[0] == '{' || trimmed[0] == '[' {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		for i := 1; ; i++ {
			var v any
			err := dec.Decode(&v)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, nil, fmt.Errorf("%s is not valid JSON (value %d: %v), so it may be cut off", name, i, err)
			}
			b, _ := json.Marshal(v)
			docs = append(docs, docText{line: 1, json: b})
		}
	} else {
		if data[len(data)-1] != '\n' {
			return nil, nil, fmt.Errorf("%s doesn't end with a newline, so it may be cut off. helm, kustomize and kubectl always end their output with one; if the file is complete, add it", name)
		}
		for i, l := range strings.Split(string(data), "\n") {
			l = strings.TrimRight(l, "\r")
			if docSep.MatchString(l) {
				if rest := strings.TrimSpace(l[3:]); rest != "" && !strings.HasPrefix(rest, "#") {
					return nil, nil, fmt.Errorf("%s:%d: a --- separator line has content after it, which kubectl refuses as an invalid document separator", name, i+1)
				}
			}
		}
		docs = splitYAML(data)
		if len(docs) > 1 && onlyComments(docs[len(docs)-1].text) && !onlyComments(docs[len(docs)-2].text) {
			return nil, nil, fmt.Errorf("%s ends with a --- separator and no document after it, so it may be cut off. helm, kustomize and kubectl don't end their output that way; if the file is complete, remove the last ---", name)
		}
	}
	var objs []*object
	var skipped []string
	for i, d := range docs {
		where := fmt.Sprintf("%s:%d", name, d.line)
		var v any
		if d.json != nil {
			dec := json.NewDecoder(bytes.NewReader(d.json))
			dec.UseNumber()
			if err := dec.Decode(&v); err != nil {
				return nil, nil, fmt.Errorf("%s: %v", where, err)
			}
		} else {
			if onlyComments(d.text) {
				continue
			}
			j, err := yamlToJSON(d.text)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: document %d is not valid YAML (%v), so the file may be cut off or not a manifest", where, i+1, err)
			}
			dec := json.NewDecoder(bytes.NewReader(j))
			dec.UseNumber()
			if err := dec.Decode(&v); err != nil {
				return nil, nil, fmt.Errorf("%s: %v", where, err)
			}
		}
		if v == nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s: document %d is %s, not a Kubernetes object. It looks like %q, which may be an error message from a failed render", where, i+1, typeName(v), firstLine(d))
		}
		got, sk, err := toObjects(m, name, d.line)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %v", where, err)
		}
		if len(got) == 1 && d.json == nil && !got[0].FromLive && !got[0].liveOnly {
			got[0].text = string(d.text)
		}
		objs = append(objs, got...)
		skipped = append(skipped, sk...)
	}
	if len(objs) > 0 {
		objs[len(objs)-1].last = true
	}
	return objs, skipped, nil
}

type docText struct {
	line int
	text []byte
	json []byte
}

// splitYAML splits a YAML stream the way kubectl does: on lines that start
// with "---" followed by nothing, whitespace or a comment.
func splitYAML(data []byte) []docText {
	var docs []docText
	lines := strings.SplitAfter(string(data), "\n")
	var cur strings.Builder
	start := 1
	for i, l := range lines {
		if docSep.MatchString(strings.TrimRight(l, "\r\n")) {
			docs = append(docs, docText{line: start, text: []byte(cur.String())})
			cur.Reset()
			start = i + 2
			continue
		}
		cur.WriteString(l)
	}
	docs = append(docs, docText{line: start, text: []byte(cur.String())})
	return docs
}

func onlyComments(b []byte) bool {
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "#") {
			return false
		}
	}
	return true
}

func firstLine(d docText) string {
	src := d.text
	if d.json != nil {
		src = d.json
	}
	for _, l := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "#") {
			if len(t) > 80 {
				t = t[:80] + "..."
			}
			return t
		}
	}
	return ""
}

func utf8Text(b []byte) bool {
	return !bytes.ContainsRune(b, 0) && strings.ToValidUTF8(string(b), "�") == string(b)
}

func typeName(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case []any:
		return "a list"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	}
	return fmt.Sprintf("%T", v)
}

func toObjects(m map[string]any, file string, line int) ([]*object, []string, error) {
	apiVersion, _ := m["apiVersion"].(string)
	kind, _ := m["kind"].(string)
	if apiVersion == "" || kind == "" {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 5 {
			keys = append(keys[:5], "...")
		}
		return nil, nil, fmt.Errorf("a document has no apiVersion or kind (its keys are %s), so it isn't a Kubernetes object. It may be an error message or a values file", strings.Join(keys, ", "))
	}
	if items, ok := m["items"].([]any); ok && strings.HasSuffix(kind, "List") {
		var out []*object
		var skipped []string
		for i, it := range items {
			im, ok := it.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("item %d of the %s is not an object", i+1, kind)
			}
			got, sk, err := toObjects(im, file, line)
			if err != nil {
				return nil, nil, fmt.Errorf("item %d of the %s: %v", i+1, kind, err)
			}
			out = append(out, got...)
			skipped = append(skipped, sk...)
		}
		return out, skipped, nil
	}
	if kind == "Status" && group(apiVersion) == "" {
		return nil, nil, fmt.Errorf("this is a Status object, which is an error response from the API server, not a manifest")
	}
	if kind == "Kustomization" || kind == "Component" && strings.HasPrefix(apiVersion, "kustomize.config.k8s.io/") {
		return nil, nil, fmt.Errorf("this is a %s, not rendered manifests: run kustomize build (or kubectl kustomize) and pass its output", kind)
	}
	meta, _ := m["metadata"].(map[string]any)
	if meta == nil {
		return nil, nil, fmt.Errorf("%s has no metadata", kind)
	}
	for _, f := range []string{"name", "namespace", "generateName"} {
		if v, _ := meta[f].(string); strings.Contains(v, "{{") {
			return nil, nil, fmt.Errorf("%s has metadata.%s %q, which is an unrendered template: run helm template and pass its output", kind, f, v)
		}
	}
	name, _ := meta["name"].(string)
	if name == "" {
		if gn, _ := meta["generateName"].(string); gn != "" {
			return nil, []string{fmt.Sprintf("%s:%d: %s with generateName %q gets a new name each time, so it is always created and never patched", file, line, kind, gn)}, nil
		}
		return nil, nil, fmt.Errorf("%s has no metadata.name", kind)
	}
	ns, _ := meta["namespace"].(string)
	o := &object{File: file, Line: line, APIVersion: apiVersion, Kind: kind, Namespace: ns, Name: name, raw: m}
	if isLive(meta, m) {
		ann, _ := meta["annotations"].(map[string]any)
		last, _ := ann["kubectl.kubernetes.io/last-applied-configuration"].(string)
		if last == "" {
			o.liveOnly = true
			return []*object{o}, nil, nil
		}
		dec := json.NewDecoder(strings.NewReader(last))
		dec.UseNumber()
		var lm map[string]any
		if err := dec.Decode(&lm); err != nil || lm == nil {
			return nil, nil, fmt.Errorf("%s %s has a last-applied-configuration annotation that isn't valid JSON", kind, name)
		}
		lmeta, _ := lm["metadata"].(map[string]any)
		if lk, _ := lm["kind"].(string); lk != kind || lmeta == nil || lmeta["name"] != name {
			return nil, nil, fmt.Errorf("%s %s has a last-applied-configuration annotation for a different object", kind, name)
		}
		o.raw = lm
		o.FromLive = true
	}
	return []*object{o}, nil, nil
}

func isLive(meta, m map[string]any) bool {
	for _, k := range []string{"uid", "resourceVersion", "managedFields"} {
		if v, ok := meta[k]; ok && v != nil {
			return true
		}
	}
	if v, ok := meta["creationTimestamp"]; ok && v != nil {
		return true
	}
	return !deepEmpty(m["status"])
}

// deepEmpty is true for a value with nothing in it, like the
// status: {loadBalancer: {}} that kubectl --dry-run=client prints.
func deepEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		for _, x := range t {
			if !deepEmpty(x) {
				return false
			}
		}
		return true
	}
	return false
}

var clusterScoped = map[string]bool{
	"Namespace": true, "Node": true, "PersistentVolume": true, "StorageClass": true,
	"ClusterRole": true, "ClusterRoleBinding": true, "CustomResourceDefinition": true,
	"PriorityClass": true, "ValidatingWebhookConfiguration": true, "MutatingWebhookConfiguration": true,
	"APIService": true, "IngressClass": true, "RuntimeClass": true, "CSIDriver": true, "VolumeAttributesClass": true,
	"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true, "MutatingAdmissionPolicy": true, "MutatingAdmissionPolicyBinding": true,
}

// checkedGroups lists the API groups each checked kind is served from.
var checkedGroups = map[string][]string{
	"Deployment":            {"apps", "extensions"},
	"ReplicaSet":            {"apps", "extensions"},
	"DaemonSet":             {"apps", "extensions"},
	"StatefulSet":           {"apps"},
	"Job":                   {"batch"},
	"Service":               {""},
	"PersistentVolumeClaim": {""},
	"PersistentVolume":      {""},
	"ConfigMap":             {""},
	"Secret":                {""},
	"RoleBinding":           {"rbac.authorization.k8s.io"},
	"ClusterRoleBinding":    {"rbac.authorization.k8s.io"},
	"StorageClass":          {"storage.k8s.io"},
	"PriorityClass":         {"scheduling.k8s.io"},
	"IngressClass":          {"networking.k8s.io"},
	"CSIDriver":             {"storage.k8s.io"},
}

func group(apiVersion string) string {
	if i := strings.LastIndex(apiVersion, "/"); i >= 0 {
		return apiVersion[:i]
	}
	return ""
}

func checked(o *object) bool {
	for _, g := range checkedGroups[o.Kind] {
		if g == group(o.APIVersion) {
			return true
		}
	}
	return false
}

// key identifies an object across the two sides. Checked kinds are matched
// by kind alone, because apps and extensions serve the same Deployment.
func key(o *object) string {
	k := o.Kind
	if !checked(o) {
		k = group(o.APIVersion) + "/" + o.Kind
	}
	return k + "|" + o.Namespace + "|" + o.Name
}

func display(o *object) string {
	if o.Namespace != "" {
		return o.Namespace + "/" + o.Name
	}
	return o.Name
}

// requiredMissing returns a field the API server requires but the object
// lacks, for the kinds wontpatch checks. A file cut off partway often loses
// these.
func requiredMissing(o *object) string {
	if !checked(o) {
		return ""
	}
	m := o.raw
	switch o.Kind {
	case "Deployment", "ReplicaSet", "DaemonSet", "StatefulSet":
		if group(o.APIVersion) == "apps" && get(m, "spec", "selector") == nil {
			return "spec.selector"
		}
		if r := podTemplateMissing(get(m, "spec", "template")); r != "" {
			return "spec.template" + r
		}
	case "Job":
		if r := podTemplateMissing(get(m, "spec", "template")); r != "" {
			return "spec.template" + r
		}
	case "PersistentVolumeClaim":
		if get(m, "spec", "resources", "requests", "storage") == nil && get(m, "spec", "dataSourceRef") == nil && get(m, "spec", "dataSource") == nil {
			return "spec.resources.requests.storage"
		}
	case "PersistentVolume":
		if get(m, "spec", "capacity", "storage") == nil {
			return "spec.capacity.storage"
		}
	case "RoleBinding", "ClusterRoleBinding":
		for _, f := range []string{"apiGroup", "kind", "name"} {
			if s, _ := get(m, "roleRef", f).(string); s == "" && f != "apiGroup" {
				return "roleRef." + f
			}
		}
	case "StorageClass":
		if s, _ := m["provisioner"].(string); s == "" {
			return "provisioner"
		}
	}
	return ""
}

func podTemplateMissing(t any) string {
	cs, _ := get(t, "spec", "containers").([]any)
	if len(cs) == 0 {
		return ".spec.containers"
	}
	for i, c := range cs {
		cm, _ := c.(map[string]any)
		if s, _ := cm["name"].(string); s == "" {
			return fmt.Sprintf(".spec.containers[%d].name", i)
		}
		if s, _ := cm["image"].(string); s == "" {
			return fmt.Sprintf(".spec.containers[%d].image", i)
		}
	}
	return ""
}

func get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

// liveWithoutAnnotation handles objects read from a cluster that have no
// last-applied annotation, so what was applied can't be told apart from
// what the cluster filled in. In NEW, or in OLD when NEW has the same object,
// they can't be compared. Other ones in OLD, like the kube-root-ca.crt
// ConfigMap every namespace gets, are dropped with a note.
func liveWithoutAnnotation(oldS, newS *side) []string {
	const why = "was read from a cluster (it has a uid, resourceVersion or status) and has no kubectl.kubernetes.io/last-applied-configuration annotation, so what was applied can't be told apart from what the cluster filled in. Objects managed by Helm or server-side apply don't have it: use helm get manifest, or render the old revision, instead"
	var out []string
	for _, n := range newS.Objects {
		if n.liveOnly {
			out = append(out, fmt.Sprintf("NEW: %s:%d: %s %s %s", n.File, n.Line, n.Kind, display(n), why))
		}
	}
	var kept []*object
	for _, o := range oldS.Objects {
		if !o.liveOnly {
			kept = append(kept, o)
			continue
		}
		if _, ok := newS.byKey[key(o)]; ok {
			out = append(out, fmt.Sprintf("OLD: %s:%d: %s %s %s", o.File, o.Line, o.Kind, display(o), why))
			continue
		}
		delete(oldS.byKey, key(o))
		oldS.skipped = append(oldS.skipped, fmt.Sprintf("%s:%d: %s %s was read from a cluster without a last-applied annotation and isn't in NEW, so it was left out", o.File, o.Line, o.Kind, display(o)))
	}
	if len(out) == 0 {
		oldS.Objects = kept
		oldS.Count = len(kept)
	}
	return out
}

// cutOff warns about a file whose last object looks cut off on a line
// break: it ends on a key with no value, or it lines up with the same object
// on the other side and stops before that one does. A file cut there still
// parses, and the dropped fields would otherwise look removed or added.
// Lines removed from, or added at, the end of the last object look the same,
// so this is a warning.
func cutOff(s, other *side, label string) []*finding {
	var out []*finding
	for _, o := range s.Objects {
		if !o.last || o.text == "" {
			continue
		}
		var why string
		lines := strings.Split(strings.TrimRight(o.text, "\n"), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			t := strings.TrimSpace(lines[i])
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if strings.HasSuffix(t, ":") || t == "-" {
				why = fmt.Sprintf("its last line, %q, is a key with no value", t)
			}
			break
		}
		if c, ok := other.byKey[key(o)]; ok && why == "" && c.text != "" {
			short, long := skeleton(o.text), skeleton(c.text)
			if tail := unmatchedTail(short, long); tail > 0 {
				why = fmt.Sprintf("it lines up with the same object in the other side's %s, which goes on for %d more lines after the point where this one stops", c.File, tail)
			}
		}
		if why == "" {
			continue
		}
		out = append(out, &finding{sev: sevWarn, Code: "cut-off", Kind: o.Kind, Namespace: o.Namespace, Name: o.Name, File: o.File, Line: o.Line,
			Message: fmt.Sprintf("is the last object in %s's file, and the file may be cut off: %s", label, why),
			Details: []string{"a file cut on a line break still parses, so fields after the cut would be compared as removed or added; if the lines really were removed or added at the end, this is fine"}})
	}
	return out
}

// unmatchedTail aligns short with long, line by line, and returns how many
// lines of long come after the line that short's last line matches, or 0 when
// short's last line has no match or short isn't shorter.
func unmatchedTail(short, long []string) int {
	n, m := len(short), len(long)
	if n == 0 || n >= m || n*m > 4_000_000 {
		return 0
	}
	if short[n-1] == long[m-1] {
		return 0
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if short[i] == long[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	// Walk the alignment and find where short's last line lands in long.
	i, j, last := 0, 0, -1
	for i < n && j < m {
		switch {
		case short[i] == long[j] && dp[i][j] == dp[i+1][j+1]+1:
			if i == n-1 {
				last = j
			}
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	if last < 0 {
		return 0
	}
	return m - 1 - last
}

// skeleton keeps each line's indentation and key, without values or
// comments, so two versions of an object with different values compare
// equal line by line.
func skeleton(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		k := t
		if strings.HasPrefix(k, "- ") {
			k = "- " + strings.TrimSpace(k[2:])
		}
		if i := strings.Index(k, ":"); i >= 0 {
			k = k[:i+1]
		} else {
			k = "(value)"
		}
		out = append(out, fmt.Sprintf("%d %s", indent, k))
	}
	return out
}

// yamlToJSON parses one YAML document with the YAML 1.1 parser kubectl uses
// (go.yaml.in/yaml/v2, copied into yaml_*.go) and turns it into JSON the way
// sigs.k8s.io/yaml does: map keys become strings.
func yamlToJSON(doc []byte) ([]byte, error) {
	var v any
	if err := Unmarshal(doc, &v); err != nil {
		return nil, err
	}
	j, err := jsonable(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(j)
}

func jsonable(v any) (any, error) {
	switch t := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			var ks string
			switch kt := k.(type) {
			case string:
				ks = kt
			case int:
				ks = strconv.Itoa(kt)
			case int64:
				ks = strconv.FormatInt(kt, 10)
			case uint64:
				ks = strconv.FormatUint(kt, 10)
			case float64:
				ks = strconv.FormatFloat(kt, 'g', -1, 32)
			case bool:
				ks = strconv.FormatBool(kt)
			default:
				return nil, fmt.Errorf("unsupported map key %v of type %T", k, k)
			}
			c, err := jsonable(x)
			if err != nil {
				return nil, err
			}
			out[ks] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			c, err := jsonable(x)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case float64:
		if t != t || t > 1e308 || t < -1e308 {
			return nil, fmt.Errorf("%v isn't a number JSON can hold", t)
		}
	}
	return v, nil
}

// ---------- comparing ----------

func compare(oldS, newS *side) *report {
	rep := &report{Version: version, Old: oldS, New: newS}
	for _, s := range oldS.skipped {
		rep.Findings = append(rep.Findings, &finding{sev: sevNote, Code: "skipped", File: "OLD", Message: s})
	}
	unchecked := map[string]int{}
	var added []string
	for _, n := range newS.Objects {
		o, ok := oldS.byKey[key(n)]
		if !ok {
			rep.Added++
			added = append(added, n.Kind+" "+display(n))
			continue
		}
		rep.Matched++
		if !checked(n) {
			unchecked[n.Kind]++
			continue
		}
		if h := hookKind(n); h != "" {
			rep.Findings = append(rep.Findings, &finding{sev: sevNote, Code: "hook", Kind: n.Kind, Namespace: n.Namespace, Name: n.Name, File: n.File, Line: n.Line,
				Message: fmt.Sprintf("is a %s hook, which is created again on each run rather than patched, so it wasn't compared", h)})
			continue
		}
		rep.Findings = append(rep.Findings, checkPair(o, n)...)
	}
	rep.Findings = append(rep.Findings, appendedAfterEnd(oldS, newS)...)
	rep.Findings = append(rep.Findings, cutOff(oldS, newS, "OLD")...)
	rep.Findings = append(rep.Findings, cutOff(newS, oldS, "NEW")...)
	var removed []*object
	for _, o := range oldS.Objects {
		if _, ok := newS.byKey[key(o)]; !ok {
			removed = append(removed, o)
		}
	}
	rep.Removed = len(removed)
	for _, o := range removed {
		if hookKind(o) != "" {
			continue
		}
		f := &finding{sev: sevWarn, Code: "removed", Kind: o.Kind, Namespace: o.Namespace, Name: o.Name, File: o.File, Line: o.Line,
			Message: "is in OLD but not in NEW, so helm upgrade or kubectl apply --prune deletes it"}
		switch o.Kind {
		case "PersistentVolumeClaim", "PersistentVolume":
			f.Details = append(f.Details, "deleting a volume claim can delete its data, depending on the reclaim policy")
		case "Namespace":
			f.Details = append(f.Details, "deleting a Namespace deletes everything in it")
		case "CustomResourceDefinition":
			f.Details = append(f.Details, "deleting a CustomResourceDefinition deletes every object of that type")
		}
		f.Details = append(f.Details, "if it was renamed or moved to another namespace, the new one is created separately; if OLD was cut off, NEW can't be compared against it")
		rep.Findings = append(rep.Findings, f)
	}
	if len(added) > 0 {
		list := added
		if len(list) > 10 {
			list = append(append([]string{}, list[:10]...), fmt.Sprintf("and %d more", len(added)-10))
		}
		rep.Findings = append(rep.Findings, &finding{sev: sevNote, Code: "added",
			Message: "only in NEW, so they are created and there is nothing to compare: " + strings.Join(list, ", ") + ". If one of them should be in OLD, OLD is incomplete"})
	}
	if len(unchecked) > 0 {
		var parts []string
		for k, c := range unchecked {
			parts = append(parts, fmt.Sprintf("%s (%d)", k, c))
		}
		sort.Strings(parts)
		rep.Findings = append(rep.Findings, &finding{sev: sevNote, Code: "unchecked-kind",
			Message: "kinds with no immutable fields that wontpatch knows of were matched but not compared: " + strings.Join(parts, ", ")})
	}
	for _, s := range newS.skipped {
		rep.Findings = append(rep.Findings, &finding{sev: sevNote, Code: "skipped", File: "NEW", Message: s})
	}
	sort.SliceStable(rep.Findings, func(i, j int) bool { return rep.Findings[i].sev > rep.Findings[j].sev })
	for _, f := range rep.Findings {
		f.Severity = sevNames[f.sev]
		switch f.sev {
		case sevError:
			rep.Errors++
		case sevWarn:
			rep.Warnings++
		default:
			rep.Notes++
		}
	}
	if rep.Findings == nil {
		rep.Findings = []*finding{}
	}
	return rep
}

// appendedAfterEnd warns when the objects that follow, in NEW, the last
// object of an OLD file are all new. That is how a new object added at the
// end of a file looks, and also how an OLD file cut off on a line break looks,
// and the two can't be told apart.
func appendedAfterEnd(oldS, newS *side) []*finding {
	var out []*finding
	for _, o := range oldS.Objects {
		if !o.last {
			continue
		}
		n, ok := newS.byKey[key(o)]
		if !ok {
			continue
		}
		var after []string
		seen, allNew := false, true
		for _, x := range newS.Objects {
			if x == n {
				seen = true
				continue
			}
			if !seen || x.File != n.File {
				continue
			}
			if _, inOld := oldS.byKey[key(x)]; inOld {
				allNew = false
				break
			}
			after = append(after, x.Kind+" "+display(x))
		}
		if !allNew || len(after) == 0 {
			continue
		}
		if len(after) > 5 {
			after = append(after[:5], fmt.Sprintf("and %d more", len(after)-5))
		}
		out = append(out, &finding{sev: sevWarn, Code: "after-end", Kind: o.Kind, Namespace: o.Namespace, Name: o.Name, File: o.File, Line: o.Line,
			Message: fmt.Sprintf("is the last object in OLD's file, and everything after it in NEW is new: %s", strings.Join(after, ", ")),
			Details: []string{"that's what objects added at the end of a file look like, and also what an OLD file cut off after this object looks like; if OLD is complete, this is fine"}})
	}
	return out
}

func hookKind(o *object) string {
	ann, _ := get(o.raw, "metadata", "annotations").(map[string]any)
	if _, ok := ann["helm.sh/hook"]; ok {
		return "Helm"
	}
	if _, ok := ann["argocd.argoproj.io/hook"]; ok {
		return "Argo CD"
	}
	return ""
}

type checker struct {
	o, n *object
	out  []*finding
}

func (c *checker) add(sev int, code, field string, oldV, newV any, msg, fix string, details ...string) {
	c.out = append(c.out, &finding{sev: sev, Code: code, Kind: c.n.Kind, Namespace: c.n.Namespace, Name: c.n.Name,
		File: c.n.File, Line: c.n.Line, Field: field, Old: show(oldV), New: show(newV), Message: msg, Fix: fix, Details: details})
}

const rejected = `kubectl apply and helm upgrade fail with "field is immutable"`

func checkPair(o, n *object) []*finding {
	c := &checker{o: o, n: n}
	om, nm := o.raw, n.raw
	switch n.Kind {
	case "Deployment", "ReplicaSet", "DaemonSet":
		c.selector(om, nm)
	case "StatefulSet":
		c.selector(om, nm)
		c.statefulSet(om, nm)
	case "Job":
		c.job(om, nm)
	case "Service":
		c.service(om, nm)
	case "PersistentVolumeClaim":
		c.pvc(om, nm)
	case "PersistentVolume":
		c.pv(om, nm)
	case "ConfigMap", "Secret":
		c.configData(om, nm)
	case "RoleBinding", "ClusterRoleBinding":
		ov, nv := roleRef(om), roleRef(nm)
		if !equal(ov, nv) {
			c.add(sevError, "roleref", "roleRef", ov, nv, "roleRef changed, and a binding's roleRef can't be changed: "+rejected,
				fmt.Sprintf("delete the %s and apply again (its subjects lose these permissions until it is recreated), or create a binding with a new name", n.Kind))
		}
	case "StorageClass":
		for _, f := range []struct{ name, def string }{{"provisioner", ""}, {"parameters", ""}, {"reclaimPolicy", "Delete"}, {"volumeBindingMode", "Immediate"}} {
			ov, nv := withDefault(om[f.name], f.def), withDefault(nm[f.name], f.def)
			if !equalKey(f.name, ov, nv) {
				c.add(sevError, "storageclass", f.name, ov, nv, f.name+" changed, and a StorageClass's "+f.name+" can't be changed: "+rejected,
					"delete the StorageClass and apply again; volumes it already provisioned keep working")
			}
		}
	case "PriorityClass":
		if ov, nv := om["value"], nm["value"]; !equal(ov, nv) {
			c.add(sevError, "priorityclass", "value", ov, nv, `value changed, and a PriorityClass's value can't be changed: kubectl apply and helm upgrade fail with "may not be changed in an update"`,
				"create a PriorityClass with a new name and point the workloads at it, or delete it and apply again")
		}
		ov, nv := withDefault(om["preemptionPolicy"], "PreemptLowerPriority"), withDefault(nm["preemptionPolicy"], "PreemptLowerPriority")
		if !equal(ov, nv) {
			c.add(sevError, "priorityclass", "preemptionPolicy", ov, nv, "preemptionPolicy changed, and it can't be changed on a PriorityClass: "+rejected,
				"create a PriorityClass with a new name and point the workloads at it, or delete it and apply again")
		}
	case "IngressClass":
		if ov, nv := get(om, "spec", "controller"), get(nm, "spec", "controller"); !equal(ov, nv) {
			c.add(sevError, "ingressclass", "spec.controller", ov, nv, "spec.controller changed, and it can't be changed on an IngressClass: "+rejected,
				"delete the IngressClass and apply again; Ingresses that name it are unserved until it is back")
		}
	case "CSIDriver":
		fix := "delete the CSIDriver and apply again, usually together with an upgrade of the driver"
		ov, nv := get(om, "spec", "attachRequired"), get(nm, "spec", "attachRequired")
		if ov == nil {
			ov = true
		}
		if nv == nil {
			nv = true
		}
		if !equal(ov, nv) {
			c.add(sevError, "csidriver", "spec.attachRequired", ov, nv, "spec.attachRequired changed, and it can't be changed on a CSIDriver: "+rejected, fix)
		}
		lm := func(m map[string]any) any {
			if l, _ := get(m, "spec", "volumeLifecycleModes").([]any); len(l) > 0 {
				return l
			}
			return []any{"Persistent"}
		}
		if ov, nv := lm(om), lm(nm); !equal(ov, nv) {
			c.add(sevError, "csidriver", "spec.volumeLifecycleModes", ov, nv, "spec.volumeLifecycleModes changed, and it can't be changed on a CSIDriver: "+rejected, fix)
		}
	}
	return c.out
}

func roleRef(m map[string]any) any {
	r, _ := m["roleRef"].(map[string]any)
	out := map[string]any{}
	for k, v := range r {
		out[k] = v
	}
	if isEmpty(out["apiGroup"]) {
		out["apiGroup"] = "rbac.authorization.k8s.io"
	}
	return out
}

func withDefault(v any, def string) any {
	if isEmpty(v) && def != "" {
		return def
	}
	return v
}

func (c *checker) selector(om, nm map[string]any) {
	if group(c.n.APIVersion) != "apps" || group(c.o.APIVersion) != "apps" {
		return
	}
	ov, nv := get(om, "spec", "selector"), get(nm, "spec", "selector")
	if equal(ov, nv) {
		return
	}
	var fix string
	switch c.n.Kind {
	case "Deployment", "ReplicaSet":
		fix = fmt.Sprintf("keep the old selector, or run kubectl delete %s %s --cascade=orphan before applying: the old pods keep serving while the new ones start, and you delete the old ReplicaSet afterwards", strings.ToLower(c.n.Kind), c.n.Name)
	case "DaemonSet":
		fix = fmt.Sprintf("keep the old selector, or run kubectl delete daemonset %s before applying, which stops its pods until the new ones start", c.n.Name)
	case "StatefulSet":
		fix = fmt.Sprintf("keep the old selector, or run kubectl delete statefulset %s --cascade=orphan, apply, and then delete the old pods one at a time so the new StatefulSet recreates them with the same volume claims", c.n.Name)
	}
	var details []string
	if lbl := changedSelectorLabels(ov, nv); lbl != "" {
		details = append(details, lbl)
	}
	c.add(sevError, "selector", "spec.selector", ov, nv, "spec.selector changed, and a "+c.n.Kind+"'s selector can't be changed: "+rejected, fix, details...)
}

func changedSelectorLabels(ov, nv any) string {
	om, _ := get(ov, "matchLabels").(map[string]any)
	nm, _ := get(nv, "matchLabels").(map[string]any)
	var parts []string
	for k, v := range nm {
		if ov, ok := om[k]; !ok {
			parts = append(parts, fmt.Sprintf("added %s=%v", k, v))
		} else if !equal(ov, v) {
			parts = append(parts, fmt.Sprintf("%s changed from %v to %v", k, ov, v))
		}
	}
	for k := range om {
		if _, ok := nm[k]; !ok {
			parts = append(parts, "removed "+k)
		}
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return ""
	}
	return "matchLabels: " + strings.Join(parts, ", ")
}

func (c *checker) statefulSet(om, nm map[string]any) {
	fix := fmt.Sprintf("delete the StatefulSet with kubectl delete statefulset %s --cascade=orphan and apply again; its pods and volume claims stay and the new StatefulSet adopts them", c.n.Name)
	ov, nv := get(om, "spec", "serviceName"), get(nm, "spec", "serviceName")
	if !equalKey("serviceName", ov, nv) {
		c.add(sevError, "statefulset", "spec.serviceName", ov, nv, "spec.serviceName changed, and it can't be changed on a StatefulSet: "+rejected, fix)
	}
	ov, nv = withDefault(get(om, "spec", "podManagementPolicy"), "OrderedReady"), withDefault(get(nm, "spec", "podManagementPolicy"), "OrderedReady")
	if !equal(ov, nv) {
		c.add(sevError, "statefulset", "spec.podManagementPolicy", ov, nv, "spec.podManagementPolicy changed, and it can't be changed on a StatefulSet: "+rejected, fix)
	}
	ovt, nvt := normVCTs(get(om, "spec", "volumeClaimTemplates")), normVCTs(get(nm, "spec", "volumeClaimTemplates"))
	if !equal(ovt, nvt) {
		details := vctDiff(ovt, nvt)
		c.add(sevError, "statefulset", "spec.volumeClaimTemplates", nil, nil, "spec.volumeClaimTemplates changed, and they can't be changed on a StatefulSet: "+rejected,
			fix+". Existing claims keep their old size and class; resize them one by one if the storage class allows expansion", details...)
	}
}

func normVCTs(v any) any {
	l, _ := v.([]any)
	var out []any
	for _, t := range l {
		tm, _ := t.(map[string]any)
		meta, _ := tm["metadata"].(map[string]any)
		nmeta := map[string]any{}
		for _, k := range []string{"name", "labels", "annotations", "namespace", "finalizers"} {
			if x, ok := meta[k]; ok {
				nmeta[k] = x
			}
		}
		spec := copyMap(tm["spec"])
		if isEmpty(spec["volumeMode"]) {
			spec["volumeMode"] = "Filesystem"
		}
		status := copyMap(tm["status"])
		setDefault(status, "phase", "Pending")
		out = append(out, map[string]any{"metadata": nmeta, "spec": spec, "status": status})
	}
	return out
}

func vctDiff(ov, nv any) []string {
	ol, _ := ov.([]any)
	nl, _ := nv.([]any)
	byName := func(l []any) map[string]any {
		m := map[string]any{}
		for _, t := range l {
			n, _ := get(t, "metadata", "name").(string)
			m[n] = t
		}
		return m
	}
	om, nm := byName(ol), byName(nl)
	var out []string
	for _, t := range nl {
		n, _ := get(t, "metadata", "name").(string)
		o, ok := om[n]
		if !ok {
			out = append(out, fmt.Sprintf("template %s was added", n))
			continue
		}
		for _, p := range diffPaths(o, t, "") {
			out = append(out, fmt.Sprintf("template %s: %s", n, p))
		}
	}
	for _, t := range ol {
		n, _ := get(t, "metadata", "name").(string)
		if _, ok := nm[n]; !ok {
			out = append(out, fmt.Sprintf("template %s was removed", n))
		}
	}
	if len(out) == 0 {
		out = append(out, "the templates are in a different order")
	}
	return out
}

func (c *checker) job(om, nm map[string]any) {
	om, nm = copyMap(om), copyMap(nm)
	fix := fmt.Sprintf("delete the Job before applying (kubectl delete job %s), give each version of the Job a new name, or make it a Helm hook so it is recreated on each upgrade", c.n.Name)
	sev := sevError
	var extra []string
	if ttl := get(om, "spec", "ttlSecondsAfterFinished"); ttl != nil {
		sev = sevWarn
		extra = append(extra, fmt.Sprintf("OLD sets ttlSecondsAfterFinished: %v, so a finished Job may already be deleted, and then the apply creates it fresh. If it still exists, the apply fails", ttl))
	}
	type jf struct {
		path []string
		def  any
	}
	for _, m := range []map[string]any{om, nm} {
		rules, _ := get(m, "spec", "podFailurePolicy", "rules").([]any)
		for _, r := range rules {
			conds, _ := get(r, "onPodConditions").([]any)
			for _, cnd := range conds {
				if cm, ok := cnd.(map[string]any); ok {
					setDefault(cm, "status", "True")
				}
			}
		}
	}
	for _, f := range []jf{
		{[]string{"spec", "completionMode"}, "NonIndexed"},
		{[]string{"spec", "podFailurePolicy"}, nil},
		{[]string{"spec", "successPolicy"}, nil},
		{[]string{"spec", "backoffLimitPerIndex"}, nil},
	} {
		ov, nv := get(om, f.path...), get(nm, f.path...)
		if f.def != nil {
			ov, nv = withDefault(ov, f.def.(string)), withDefault(nv, f.def.(string))
		}
		if !equal(ov, nv) {
			c.add(sev, "job", strings.Join(f.path, "."), ov, nv, strings.Join(f.path, ".")+" changed, and it can't be changed on a Job: "+rejected, fix, extra...)
		}
	}
	oc, nc := get(om, "spec", "completions"), get(nm, "spec", "completions")
	if !equal(oc, nc) {
		indexed := withDefault(get(nm, "spec", "completionMode"), "NonIndexed") == "Indexed"
		op, np := withDefaultNum(get(om, "spec", "parallelism"), "1"), withDefaultNum(get(nm, "spec", "parallelism"), "1")
		elastic := indexed && equal(nc, np) && equal(oc, op)
		if !elastic {
			msg := "spec.completions changed, and it can't be changed on a Job: " + rejected
			if indexed {
				msg = "spec.completions changed without spec.parallelism changing to the same value, which an Indexed Job requires: kubectl apply and helm upgrade fail"
			}
			c.add(sev, "job", "spec.completions", oc, nc, msg, fix, extra...)
		}
	}
	if ov, nv := get(om, "spec", "selector"), get(nm, "spec", "selector"); (ov != nil || nv != nil) && !equal(ov, nv) {
		c.add(sev, "job", "spec.selector", ov, nv, "spec.selector changed, and it can't be changed on a Job: "+rejected, fix, extra...)
	}
	if ov, nv := get(om, "spec", "managedBy"), get(nm, "spec", "managedBy"); !equalKey("managedBy", ov, nv) {
		c.add(sev, "job", "spec.managedBy", ov, nv, "spec.managedBy changed, and it can't be changed on a Job: "+rejected, fix, extra...)
	}
	ot, nt := normPodTemplate(get(om, "spec", "template")), normPodTemplate(get(nm, "spec", "template"))
	if get(om, "spec", "manualSelector") != true && get(nm, "spec", "manualSelector") != true {
		for _, t := range []any{ot, nt} {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			meta, _ := tm["metadata"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
				tm["metadata"] = meta
			}
			labels, _ := meta["labels"].(map[string]any)
			if labels == nil {
				labels = map[string]any{}
				meta["labels"] = labels
			}
			setDefault(labels, "job-name", c.n.Name)
			setDefault(labels, "batch.kubernetes.io/job-name", c.n.Name)
		}
	}
	if equal(ot, nt) {
		return
	}
	paths := diffPaths(ot, nt, "")
	suspended := get(om, "spec", "suspend") == true && get(nm, "spec", "suspend") == true
	if suspended && onlySchedulingPaths(paths) {
		c.add(sevNote, "job", "spec.template", nil, nil,
			"spec.template changes only scheduling fields, which a suspended Job that has never started accepts. If the Job has started, the apply fails", "")
		return
	}
	details := append([]string{}, paths...)
	if len(details) > 8 {
		details = append(details[:8], fmt.Sprintf("and %d more", len(paths)-8))
	}
	details = append(details, extra...)
	c.add(sev, "job", "spec.template", nil, nil, "spec.template changed, and a Job's pod template can't be changed: "+rejected, fix, details...)
}

func withDefaultNum(v any, def string) any {
	if v == nil {
		return json.Number(def)
	}
	return v
}

var schedulingPaths = regexp.MustCompile(`^spec\.(nodeSelector|tolerations|affinity|schedulingGates)\b|^metadata\.(labels|annotations)\b`)

func onlySchedulingPaths(paths []string) bool {
	for _, p := range paths {
		if !schedulingPaths.MatchString(p) {
			return false
		}
	}
	return true
}

func (c *checker) service(om, nm map[string]any) {
	fix := "delete the Service and apply again (it gets a new cluster IP; clients that use its DNS name follow), or stop setting the field"
	ip := func(m map[string]any) string {
		s, _ := get(m, "spec", "clusterIP").(string)
		if s == "" {
			if l, _ := get(m, "spec", "clusterIPs").([]any); len(l) > 0 {
				s, _ = l[0].(string)
			}
		}
		return s
	}
	typ := func(m map[string]any) string {
		s, _ := get(m, "spec", "type").(string)
		if s == "" {
			return "ClusterIP"
		}
		return s
	}
	oip, nip := ip(om), ip(nm)
	if typ(om) != "ExternalName" && typ(nm) != "ExternalName" && nip != "" && oip != nip {
		switch {
		case oip != "":
			c.add(sevError, "service", "spec.clusterIP", oip, nip, `spec.clusterIP changed, and a Service's cluster IP can't change once set: kubectl apply and helm upgrade fail with "may not change once set"`, fix)
		case nip == "None":
			c.add(sevError, "service", "spec.clusterIP", "(assigned by the cluster)", nip, `spec.clusterIP is now None, which makes the Service headless, but the cluster gave it an IP when it was created: kubectl apply and helm upgrade fail with "may not change once set"`, fix)
		default:
			c.add(sevWarn, "service", "spec.clusterIP", "(assigned by the cluster)", nip, "spec.clusterIP is now set, but the cluster gave the Service an IP when it was created. The apply fails unless it is the same address", fix)
		}
	}
	fam := func(m map[string]any) string {
		l, _ := get(m, "spec", "ipFamilies").([]any)
		if len(l) == 0 {
			return ""
		}
		s, _ := l[0].(string)
		return s
	}
	if typ(om) == "LoadBalancer" && typ(nm) == "LoadBalancer" {
		if ov, nv := get(om, "spec", "loadBalancerClass"), get(nm, "spec", "loadBalancerClass"); !equalKey("loadBalancerClass", ov, nv) {
			c.add(sevError, "service", "spec.loadBalancerClass", ov, nv, `spec.loadBalancerClass changed on a LoadBalancer Service, and it can't change once the Service exists: kubectl apply and helm upgrade fail with "may not change once set"`, fix)
		}
	}
	of, nf := fam(om), fam(nm)
	if typ(nm) != "ExternalName" && nf != "" && of != nf {
		if of != "" {
			c.add(sevError, "service", "spec.ipFamilies", of, nf, "the primary IP family in spec.ipFamilies changed, and it can't change while the Service keeps its cluster IP: kubectl apply and helm upgrade fail", fix)
		} else {
			sev := sevWarn
			if nf == "IPv4" {
				sev = sevNote
			}
			c.add(sev, "service", "spec.ipFamilies", "(chosen by the cluster)", nf, "spec.ipFamilies is now set. The cluster chose the primary family when the Service was created, and the apply fails if it is not "+nf, fix)
		}
	}
}

func (c *checker) pvc(om, nm map[string]any) {
	fix := "a claim's spec can't be changed after creation: create a claim with a new name and copy the data, or keep the old values"
	os, ns := copyMap(om["spec"]), copyMap(nm["spec"])
	for _, s := range []map[string]any{os, ns} {
		// The API server copies dataSource and dataSourceRef into each other.
		if s["dataSource"] != nil && s["dataSourceRef"] == nil {
			s["dataSourceRef"] = deepCopy(s["dataSource"])
		} else if s["dataSourceRef"] != nil && s["dataSource"] == nil && get(s, "dataSourceRef", "namespace") == nil {
			s["dataSource"] = deepCopy(s["dataSourceRef"])
		}
	}
	oq, nq := get(os, "resources", "requests", "storage"), get(ns, "resources", "requests", "storage")
	if ov, nv := os["volumeAttributesClassName"], ns["volumeAttributesClassName"]; !equalKey("volumeAttributesClassName", ov, nv) {
		c.add(sevWarn, "pvc", "spec.volumeAttributesClassName", ov, nv, "spec.volumeAttributesClassName changed. This works only if the claim is bound and the cluster has VolumeAttributesClass enabled; otherwise the apply fails", "")
	}
	for _, s := range []map[string]any{os, ns} {
		if r, ok := s["resources"].(map[string]any); ok {
			r = copyMap(r)
			delete(r, "requests")
			s["resources"] = r
		}
		delete(s, "volumeAttributesClassName")
		if isEmpty(s["volumeMode"]) {
			s["volumeMode"] = "Filesystem"
		}
	}
	if os["storageClassName"] == nil && !isEmpty(ns["storageClassName"]) {
		c.add(sevWarn, "pvc", "spec.storageClassName", "(not set)", ns["storageClassName"],
			"spec.storageClassName is now set. Most clusters give a claim the default StorageClass when it is created, and then this apply fails unless the names match", fix)
		os["storageClassName"] = ns["storageClassName"]
	}
	if !equal(os, ns) {
		details := diffPaths(os, ns, "spec.")
		c.add(sevError, "pvc", "spec", nil, nil, `spec changed, and a claim's spec can't change after creation except resources.requests and volumeAttributesClassName: kubectl apply and helm upgrade fail with "spec is immutable after creation"`, fix, details...)
	}
	if oq != nil && nq != nil && !equal(oq, nq) {
		oqr, ok1 := parseQuantity(fmt.Sprint(oq))
		nqr, ok2 := parseQuantity(fmt.Sprint(nq))
		switch {
		case !ok1 || !ok2:
			c.add(sevWarn, "pvc", "spec.resources.requests.storage", oq, nq, "the storage request changed, and one of the values isn't a quantity wontpatch can read", "")
		case nqr.Cmp(oqr) < 0:
			c.add(sevError, "pvc", "spec.resources.requests.storage", oq, nq, `the storage request shrank, and a claim can't shrink: kubectl apply and helm upgrade fail with "field can not be less than status.capacity"`,
				"keep the old size, or create a smaller claim with a new name and copy the data")
		default:
			c.add(sevWarn, "pvc", "spec.resources.requests.storage", oq, nq, "the storage request grew. This works only if the claim is bound and its StorageClass sets allowVolumeExpansion: true; otherwise the apply fails",
				"check the StorageClass with kubectl get storageclass")
		}
	}
}

var pvMutable = map[string]bool{"capacity": true, "accessModes": true, "persistentVolumeReclaimPolicy": true, "storageClassName": true,
	"mountOptions": true, "claimRef": true, "nodeAffinity": true, "volumeMode": true, "volumeAttributesClassName": true}

func (c *checker) pv(om, nm map[string]any) {
	fix := "delete the PersistentVolume and create it again (set its reclaim policy to Retain first so the data stays), or keep the old values"
	os, ns := copyMap(om["spec"]), copyMap(nm["spec"])
	dropFalse(os)
	dropFalse(ns)
	src := func(s map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range s {
			if !pvMutable[k] {
				out[k] = v
			}
		}
		return out
	}
	if !equal(src(os), src(ns)) {
		c.add(sevError, "pv", "spec (volume source)", nil, nil, `the volume source changed, and it can't change after creation: kubectl apply and helm upgrade fail with "spec.persistentvolumesource is immutable after creation"`, fix, diffPaths(src(os), src(ns), "spec.")...)
	}
	ov, nv := withDefault(os["volumeMode"], "Filesystem"), withDefault(ns["volumeMode"], "Filesystem")
	if !equal(ov, nv) {
		c.add(sevError, "pv", "spec.volumeMode", ov, nv, "spec.volumeMode changed, and it can't change: "+rejected, fix)
	}
	if !isEmpty(os["nodeAffinity"]) && !equal(os["nodeAffinity"], ns["nodeAffinity"]) {
		c.add(sevError, "pv", "spec.nodeAffinity", nil, nil, "spec.nodeAffinity changed, and once set it can't change: "+rejected, fix, diffPaths(os["nodeAffinity"], ns["nodeAffinity"], "spec.nodeAffinity.")...)
	}
}

func (c *checker) configData(om, nm map[string]any) {
	fix := fmt.Sprintf("give the %s a new name for each version of its content (Kustomize's generators and many charts add a hash suffix) and point the workloads at it, or delete it and apply again", c.n.Kind)
	if c.n.Kind == "Secret" {
		ot, nt := withDefault(om["type"], "Opaque"), withDefault(nm["type"], "Opaque")
		if !equal(ot, nt) {
			c.add(sevError, "secret-type", "type", ot, nt, "type changed, and a Secret's type can't be changed: "+rejected, "delete the Secret and apply again, or create one with a new name")
		}
	}
	if om["immutable"] != true {
		return
	}
	if nm["immutable"] != true {
		c.add(sevError, "immutable", "immutable", true, nm["immutable"], fmt.Sprintf(`OLD marks the %s immutable: true, and that can't be undone: kubectl apply and helm upgrade fail with "field is immutable when immutable is set"`, c.n.Kind), fix)
		return
	}
	od, nd := configContent(c.n.Kind, om), configContent(c.n.Kind, nm)
	if !equal(od, nd) {
		var keys []string
		for _, p := range diffPaths(od, nd, "") {
			keys = append(keys, p)
		}
		c.add(sevError, "immutable", "data", nil, nil, fmt.Sprintf(`the %s is marked immutable: true and its data changed: kubectl apply and helm upgrade fail with "field is immutable when immutable is set"`, c.n.Kind), fix, redactValues(keys)...)
	}
}

// configContent merges stringData into data the way the API server does,
// so the two forms of the same Secret compare equal.
func configContent(kind string, m map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range []string{"data", "binaryData"} {
		d, _ := m[f].(map[string]any)
		if len(d) > 0 {
			out[f] = copyMap(d)
		}
	}
	if kind == "Secret" {
		d, _ := m["stringData"].(map[string]any)
		for k, v := range d {
			data, _ := out["data"].(map[string]any)
			if data == nil {
				data = map[string]any{}
				out["data"] = data
			}
			data[k] = base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(v)))
		}
	}
	return out
}

// redactValues keeps the changed key names and drops the values, which may be
// secrets.
func redactValues(paths []string) []string {
	var out []string
	for _, p := range paths {
		if i := strings.Index(p, ":"); i >= 0 {
			p = p[:i]
		}
		out = append(out, p+" changed")
	}
	return out
}

// ---------- normalizing ----------

// normPodTemplate fills in the defaults the API server sets on a pod
// template, so a manifest that spells out a default compares equal to one
// that leaves it out.
func normPodTemplate(t any) any {
	tm := deepCopy(t)
	m, ok := tm.(map[string]any)
	if !ok {
		return tm
	}
	if meta, ok := m["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		return m
	}
	setDefault(spec, "dnsPolicy", "ClusterFirst")
	setDefault(spec, "restartPolicy", "Always")
	setDefault(spec, "schedulerName", "default-scheduler")
	setDefault(spec, "terminationGracePeriodSeconds", json.Number("30"))
	setDefault(spec, "securityContext", map[string]any{})
	if sa, _ := spec["serviceAccountName"].(string); sa != "" {
		setDefault(spec, "serviceAccount", sa)
	} else if sa, _ := spec["serviceAccount"].(string); sa != "" {
		spec["serviceAccountName"] = sa
	}
	hostNet := spec["hostNetwork"] == true
	for _, list := range []string{"initContainers", "containers"} {
		cs, _ := spec[list].([]any)
		for _, c := range cs {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			setDefault(cm, "terminationMessagePath", "/dev/termination-log")
			setDefault(cm, "terminationMessagePolicy", "File")
			if img, _ := cm["image"].(string); img != "" {
				setDefault(cm, "imagePullPolicy", defaultPullPolicy(img))
			}
			if ports, ok := cm["ports"].([]any); ok {
				for _, p := range ports {
					if pm, ok := p.(map[string]any); ok {
						setDefault(pm, "protocol", "TCP")
						if hostNet {
							setDefault(pm, "hostPort", pm["containerPort"])
						}
					}
				}
			}
			for _, pr := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
				if p, ok := cm[pr].(map[string]any); ok {
					setDefault(p, "timeoutSeconds", json.Number("1"))
					setDefault(p, "periodSeconds", json.Number("10"))
					setDefault(p, "successThreshold", json.Number("1"))
					setDefault(p, "failureThreshold", json.Number("3"))
					if h, ok := p["httpGet"].(map[string]any); ok {
						setDefault(h, "scheme", "HTTP")
						setDefault(h, "path", "/")
					}
				}
			}
			if lc, ok := cm["lifecycle"].(map[string]any); ok {
				for _, hk := range []string{"postStart", "preStop"} {
					if h, ok := get(lc, hk, "httpGet").(map[string]any); ok {
						setDefault(h, "scheme", "HTTP")
						setDefault(h, "path", "/")
					}
				}
			}
			if env, ok := cm["env"].([]any); ok {
				for _, e := range env {
					if fr, ok := get(e, "valueFrom", "fieldRef").(map[string]any); ok {
						setDefault(fr, "apiVersion", "v1")
					}
					if rr, ok := get(e, "valueFrom", "resourceFieldRef").(map[string]any); ok {
						setDefault(rr, "divisor", "0")
					}
				}
			}
		}
	}
	if vols, ok := spec["volumes"].([]any); ok {
		for _, v := range vols {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			for _, src := range []string{"configMap", "secret", "projected", "downwardAPI"} {
				if s, ok := vm[src].(map[string]any); ok {
					setDefault(s, "defaultMode", json.Number("420"))
				}
			}
			if s, ok := get(vm, "downwardAPI", "items").([]any); ok {
				for _, it := range s {
					if fr, ok := get(it, "fieldRef").(map[string]any); ok {
						setDefault(fr, "apiVersion", "v1")
					}
					if rr, ok := get(it, "resourceFieldRef").(map[string]any); ok {
						setDefault(rr, "divisor", "0")
					}
				}
			}
			if _, ok := vm["emptyDir"]; ok && vm["emptyDir"] == nil {
				vm["emptyDir"] = map[string]any{}
			}
			if srcs, ok := get(vm, "projected", "sources").([]any); ok {
				for _, src := range srcs {
					if t, ok := get(src, "serviceAccountToken").(map[string]any); ok {
						setDefault(t, "expirationSeconds", json.Number("3600"))
					}
				}
			}
			if vs, ok := get(vm, "ephemeral", "volumeClaimTemplate", "spec").(map[string]any); ok {
				setDefault(vs, "volumeMode", "Filesystem")
			}
			if im, ok := vm["image"].(map[string]any); ok {
				if ref, _ := im["reference"].(string); ref != "" {
					setDefault(im, "pullPolicy", defaultPullPolicy(ref))
				}
			}
			if is, ok := vm["iscsi"].(map[string]any); ok {
				setDefault(is, "iscsiInterface", "default")
			}
			if rb, ok := vm["rbd"].(map[string]any); ok {
				setDefault(rb, "pool", "rbd")
				setDefault(rb, "user", "admin")
				setDefault(rb, "keyring", "/etc/ceph/keyring")
			}
			if si, ok := vm["scaleIO"].(map[string]any); ok {
				setDefault(si, "fsType", "xfs")
				setDefault(si, "storageMode", "ThinProvisioned")
			}
			if ad, ok := vm["azureDisk"].(map[string]any); ok {
				setDefault(ad, "cachingMode", "ReadWrite")
				setDefault(ad, "fsType", "ext4")
				setDefault(ad, "kind", "Shared")
			}
		}
	}
	dropFalse(spec)
	return m
}

// Non-pointer booleans in the pod spec, where false and unset are the same.
var plainBools = map[string]bool{"hostNetwork": true, "hostPID": true, "hostIPC": true, "readOnly": true, "stdin": true, "stdinOnce": true, "tty": true}

func dropFalse(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if plainBools[k] && x == false {
				delete(t, k)
				continue
			}
			if k == "csi" {
				continue // CSIVolumeSource.ReadOnly is a pointer
			}
			dropFalse(x)
		}
	case []any:
		for _, x := range t {
			dropFalse(x)
		}
	}
}

var (
	imageNameRe   = regexp.MustCompile(`^(?:((?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))*(?::[0-9]+)?)/)?([a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*)$`)
	imageTagRe    = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
	imageDigestRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}$`)
)

// defaultPullPolicy follows the API server: Always for the latest tag or an
// image with neither tag nor digest, IfNotPresent otherwise, and
// IfNotPresent when the name doesn't parse as an image reference.
func defaultPullPolicy(image string) string {
	name, digest := image, ""
	if i := strings.Index(image, "@"); i >= 0 {
		name, digest = image[:i], image[i+1:]
		if !imageDigestRe.MatchString(digest) {
			return "IfNotPresent"
		}
	}
	tag := ""
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i+1:], "/") {
		name, tag = name[:i], name[i+1:]
		if !imageTagRe.MatchString(tag) {
			return "IfNotPresent"
		}
	}
	if len(name) > 255 || !imageNameRe.MatchString(name) {
		return "IfNotPresent"
	}
	if tag == "latest" || tag == "" && digest == "" {
		return "Always"
	}
	return "IfNotPresent"
}

func setDefault(m map[string]any, k string, v any) {
	if x, ok := m[k]; !ok || isEmpty(x) {
		m[k] = v
	}
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}

func copyMap(v any) map[string]any {
	m, _ := deepCopy(v).(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = deepCopy(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopy(x)
		}
		return out
	}
	return v
}

// canon writes values the way the API server compares them. JSON null,
// empty lists and empty strings count as unset, except strings that are
// pointers in the Go types. Empty objects count as unset only for maps and
// for structs that aren't pointers; an empty pointer struct like
// securityContext: {} is set. Inside string maps such as labels and data, a
// key with an empty value is kept. Numbers and quantities get one form.
func canon(v any, parentKey string) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		inMap := stringMaps[parentKey] || quantityMaps[parentKey]
		for k, x := range t {
			if x == nil {
				continue
			}
			if quantityMaps[parentKey] || quantityKeys[k] {
				if q, ok := parseQuantity(fmt.Sprint(x)); ok {
					out[k] = "q:" + q.RatString()
					continue
				}
			}
			c := canon(x, k)
			if !inMap {
				switch cv := c.(type) {
				case string:
					if cv == "" && !pointerStrings[k] {
						continue
					}
				case []any:
					if len(cv) == 0 {
						continue
					}
				case map[string]any:
					if len(cv) == 0 && (stringMaps[k] || quantityMaps[k] || plainStructs[k]) {
						continue
					}
				}
			}
			out[k] = c
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, canon(x, ""))
		}
		return out
	case json.Number:
		if r, ok := new(big.Rat).SetString(t.String()); ok {
			return "n:" + r.RatString()
		}
		return t.String()
	}
	return v
}

// Keys whose values are Go maps of strings.
var stringMaps = map[string]bool{"labels": true, "annotations": true, "matchLabels": true, "nodeSelector": true,
	"data": true, "binaryData": true, "stringData": true, "parameters": true}

// Keys whose values are structs that aren't pointers, where {} and unset are
// the same.
var plainStructs = map[string]bool{"metadata": true, "spec": true, "template": true, "resources": true}

// String fields that are pointers, where "" and unset differ.
var pointerStrings = map[string]bool{"storageClassName": true, "volumeAttributesClassName": true, "loadBalancerClass": true,
	"managedBy": true, "runtimeClassName": true, "procMount": true, "podReplacementPolicy": true, "preemptionPolicy": true}

var quantityMaps = map[string]bool{"requests": true, "limits": true, "capacity": true}
var quantityKeys = map[string]bool{"sizeLimit": true, "divisor": true}

func equal(a, b any) bool {
	return equalKey("", a, b)
}

// equalKey compares two values of the field named key.
func equalKey(key string, a, b any) bool {
	return reflect.DeepEqual(canon(map[string]any{key: a}, ""), canon(map[string]any{key: b}, ""))
}

var quantityRe = regexp.MustCompile(`^([+-]?[0-9]*\.?[0-9]+)(?:(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E)|[eE]([+-]?[0-9]+))?$`)

func parseQuantity(s string) (*big.Rat, bool) {
	m := quantityRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return nil, false
	}
	mult := new(big.Rat).SetInt64(1)
	pow := func(base, exp int64) *big.Rat {
		return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(base), big.NewInt(exp), nil))
	}
	switch m[2] {
	case "Ki":
		mult = pow(2, 10)
	case "Mi":
		mult = pow(2, 20)
	case "Gi":
		mult = pow(2, 30)
	case "Ti":
		mult = pow(2, 40)
	case "Pi":
		mult = pow(2, 50)
	case "Ei":
		mult = pow(2, 60)
	case "n":
		mult = new(big.Rat).Inv(pow(10, 9))
	case "u":
		mult = new(big.Rat).Inv(pow(10, 6))
	case "m":
		mult = new(big.Rat).Inv(pow(10, 3))
	case "k":
		mult = pow(10, 3)
	case "M":
		mult = pow(10, 6)
	case "G":
		mult = pow(10, 9)
	case "T":
		mult = pow(10, 12)
	case "P":
		mult = pow(10, 15)
	case "E":
		mult = pow(10, 18)
	}
	if m[3] != "" {
		e, err := strconv.ParseInt(m[3], 10, 64)
		if err != nil || e > 30 || e < -30 {
			return nil, false
		}
		if e >= 0 {
			mult = pow(10, e)
		} else {
			mult = new(big.Rat).Inv(pow(10, -e))
		}
	}
	return r.Mul(r, mult), true
}

// diffPaths lists the leaf paths that differ between two values.
func diffPaths(a, b any, prefix string) []string {
	var out []string
	var walk func(x, y any, p string)
	walk = func(x, y any, p string) {
		if equal(x, y) {
			return
		}
		xm, xok := canonMap(x)
		ym, yok := canonMap(y)
		if xok && yok {
			keys := map[string]bool{}
			for k := range xm {
				keys[k] = true
			}
			for k := range ym {
				keys[k] = true
			}
			sorted := make([]string, 0, len(keys))
			for k := range keys {
				sorted = append(sorted, k)
			}
			sort.Strings(sorted)
			for _, k := range sorted {
				walk(xm[k], ym[k], joinPath(p, k))
			}
			return
		}
		xl, xok := x.([]any)
		yl, yok := y.([]any)
		if xok && yok && len(xl) == len(yl) {
			for i := range xl {
				walk(xl[i], yl[i], fmt.Sprintf("%s[%d]", p, i))
			}
			return
		}
		out = append(out, fmt.Sprintf("%s: %s -> %s", strings.TrimSuffix(p, "."), show(x), show(y)))
	}
	walk(a, b, prefix)
	return out
}

func canonMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if ok {
		return m, true
	}
	if v == nil {
		return map[string]any{}, false
	}
	return nil, false
}

func joinPath(p, k string) string {
	if strings.ContainsAny(k, "./") {
		return strings.TrimSuffix(p, ".") + "[" + k + "]"
	}
	if p == "" || strings.HasSuffix(p, ".") {
		return p + k
	}
	return p + "." + k
}

func show(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	s := string(b)
	if s == "null" {
		return "(not set)"
	}
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// ---------- output ----------

func printText(w io.Writer, r *report) {
	fmt.Fprintf(w, "wontpatch: OLD %s (%s), NEW %s (%s)\n", r.Old.Label, plural(r.Old.Count, "object"), r.New.Label, plural(r.New.Count, "object"))
	fmt.Fprintf(w, "  %d matched, %d only in NEW (created, nothing to compare), %d only in OLD\n", r.Matched, r.Added, r.Removed)
	for _, f := range r.Findings {
		head := fmt.Sprintf("  %-5s ", f.Severity)
		if f.Kind != "" {
			loc := f.Kind + " " + f.Name
			if f.Namespace != "" {
				loc += " (namespace " + f.Namespace + ")"
			}
			fmt.Fprintf(w, "%s%s, %s:%d\n", head, loc, f.File, f.Line)
			msg := f.Message
			if f.Code == "removed" || f.Code == "hook" || f.Code == "after-end" || f.Code == "cut-off" {
				msg = "it " + msg
			}
			fmt.Fprintf(w, "        %s\n", msg)
		} else {
			fmt.Fprintf(w, "%s%s\n", head, f.Message)
		}
		if f.Old != "" || f.New != "" {
			fmt.Fprintf(w, "        old: %s\n", orNotSet(f.Old))
			fmt.Fprintf(w, "        new: %s\n", orNotSet(f.New))
		}
		for _, d := range f.Details {
			fmt.Fprintf(w, "        %s\n", d)
		}
		if f.Fix != "" {
			fmt.Fprintf(w, "        fix: %s\n", f.Fix)
		}
	}
	switch {
	case r.Errors+r.Warnings > 0:
		fmt.Fprintf(w, "  %s, %s.\n", plural(r.Errors, "error"), plural(r.Warnings, "warning"))
	case r.Matched == 0:
		fmt.Fprintln(w, "  No object is in both OLD and NEW, so nothing was compared.")
	default:
		fmt.Fprintf(w, "  No immutable-field changes in the %s present in both.\n", plural(r.Matched, "object"))
	}
}

func orNotSet(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
