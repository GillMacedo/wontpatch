# Architecture

wontpatch is one Go program in `plugin/scripts`, with no dependencies outside the repository, so the installed plugin builds with `go build` on its `.go` files and no module.

## Flow

1. **Load each side** (`loadSide`, `parseFile`). A side is a file, a directory or standard input. YAML is split on `---` lines the way kubectl splits it, and each document is parsed with go.yaml.in/yaml/v2, the YAML 1.1 parser behind kubectl's `sigs.k8s.io/yaml`, copied into `yaml_*.go`. JSON files are read as a stream of values. `List` objects are expanded. Live objects (with a `uid`, `resourceVersion`, `managedFields`, `creationTimestamp` or `status`) are replaced by their `kubectl.kubernetes.io/last-applied-configuration` annotation.
2. **Refuse what can't be trusted.** Any of these stops the run with exit 2 and no result: empty input, invalid YAML or JSON, documents that aren't Kubernetes objects, charts, kustomizations, unrendered templates, a missing final newline, a trailing `---`, required fields missing from checked kinds, duplicates, content on a `---` line, `kind: Status` error responses, files with no objects inside a directory, and live objects without the annotation that NEW also has (`liveWithoutAnnotation`). A file cut on a line break can still parse, so `appendedAfterEnd` and `cutOff` warn when the end of a file looks cut off.
3. **Match** objects by kind, namespace and name (`key`). Kinds with rules ignore the API group version, so `apps/v1` and `extensions/v1beta1` Deployments match. `--namespace` fills in namespaces the render left out.
4. **Compare** (`checkPair`). Each kind has a function for its immutable fields. Values pass through `canon`, which treats empty values the way the API server's Go types do (an empty pointer struct is set, an empty string in a label map is kept, an empty list is unset) and writes numbers and quantities in one form, and Job pod templates pass through `normPodTemplate`, which fills in the defaults the API server sets.
5. **Report** findings sorted by severity, as text or JSON, and exit 0, 1 or 2.

## Where the rules come from

Each rule was written from the API server's validation and then checked by applying an old and a new manifest to kube-apiserver v1.37.0. `tests/cases` holds those pairs, and `tests/groundtruth_test.go` applies them again with client-side and server-side apply when `scripts/apiserver.sh` has started a server. A rule only reports ERROR when the server rejected every case for it. When the result depends on the cluster, such as growing a PVC or a Service IP the cluster chose, it reports WARN.

## Tests

- `plugin/scripts/wontpatch_test.go`: flags, exit codes, untrusted input on each side, cuts at every byte, live objects, Secret redaction and helpers.
- `tests/`: builds the binary and runs it on `tests/cases`, the examples, real chart renders in `tests/testdata/charts`, and the plugin copied outside the module. With an API server, it checks every case and the examples against real applies.
- `scripts/chart-sweep.sh`: compares wontpatch with real `helm upgrade` over consecutive chart versions. It isn't part of CI because it needs Helm and a chart repository.
