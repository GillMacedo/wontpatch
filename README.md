# wontpatch: catch "field is immutable" before helm upgrade and kubectl apply

[![CI](https://github.com/GillMacedo/wontpatch/actions/workflows/ci.yml/badge.svg)](https://github.com/GillMacedo/wontpatch/actions/workflows/ci.yml) [![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A Claude Code skill and Go CLI for anyone who ships Kubernetes manifests with Helm, Kustomize, kubectl, Argo CD or Flux. It compares what is deployed with what you are about to apply and reports the changes the API server will reject, such as a new Deployment selector, a resized StatefulSet volumeClaimTemplate or a new image in a Job, before the upgrade fails in the middle of a release.**

Some fields can't change once an object exists. `helm template`, `kubeconform` and a diff all pass, and then `helm upgrade` stops with `cannot patch "web" with kind Deployment: spec.selector: Invalid value: ...: field is immutable`, half the release applied and the rest not. A chart that adds a label to its selector, a bigger disk in a StatefulSet's volumeClaimTemplates, a migration Job with a new image tag, a RoleBinding that points at another role: each needs a delete first, and you find out at deploy time. `wontpatch` reads the two rendered manifest sets offline and tells you which objects will be rejected, which field, and what to do about it.

## What you can ask Claude

- "Will upgrading this Helm release to chart 4.0.0 work, or will it fail with field is immutable?"
- "Check this PR's manifest changes for anything kubectl apply will reject."
- "helm upgrade failed with cannot patch StatefulSet: spec.volumeClaimTemplates is immutable. What now?"
- "Which objects do I have to delete before this kustomize change can be applied?"

## Install

As a Claude Code plugin:

    claude plugin marketplace add GillMacedo/wontpatch
    claude plugin install wontpatch@wontpatch-marketplace

Or run it directly with Go 1.22 or newer. It has no dependencies outside this repository:

    go run github.com/GillMacedo/wontpatch/plugin/scripts@v0.1.0 OLD NEW

## Example

`examples/shop/v1` is a small made-up service. `examples/shop/v2-breaking` is a later version with one rejected change of each common kind. `--fail-on none` keeps the exit code at 0 for this demo:

    $ go run ./plugin/scripts examples/shop/v1 examples/shop/v2-breaking --fail-on none
    wontpatch: OLD examples/shop/v1 (9 objects), NEW examples/shop/v2-breaking (9 objects)
      9 matched, 0 only in NEW (created, nothing to compare), 0 only in OLD
      ERROR Service web (namespace shop), examples/shop/v2-breaking/app.yaml:8
            spec.clusterIP is now None, which makes the Service headless, but the cluster gave it an IP when it was created: kubectl apply and helm upgrade fail with "may not change once set"
            old: (assigned by the cluster)
            new: None
            fix: delete the Service and apply again (it gets a new cluster IP; clients that use its DNS name follow), or stop setting the field
      ERROR Deployment web (namespace shop), examples/shop/v2-breaking/app.yaml:22
            spec.selector changed, and a Deployment's selector can't be changed: kubectl apply and helm upgrade fail with "field is immutable"
            old: {"matchLabels":{"app":"web"}}
            new: {"matchLabels":{"app":"web","version":"v2"}}
            matchLabels: added version=v2
            fix: keep the old selector, or run kubectl delete deployment web --cascade=orphan before applying: the old pods keep serving while the new ones start, and you delete the old ReplicaSet afterwards
      ERROR StatefulSet db (namespace shop), examples/shop/v2-breaking/app.yaml:46
            spec.volumeClaimTemplates changed, and they can't be changed on a StatefulSet: kubectl apply and helm upgrade fail with "field is immutable"
            template data: spec.resources.requests.storage: 10Gi -> 20Gi
            fix: delete the StatefulSet with kubectl delete statefulset db --cascade=orphan and apply again; its pods and volume claims stay and the new StatefulSet adopts them. Existing claims keep their old size and class; resize them one by one if the storage class allows expansion
      ERROR Job migrate (namespace shop), examples/shop/v2-breaking/app.yaml:74
            spec.template changed, and a Job's pod template can't be changed: kubectl apply and helm upgrade fail with "field is immutable"
            spec.containers[0].image: ghcr.io/example/shop-migrate:1.4.0 -> ghcr.io/example/shop-migrate:2.0.0
            fix: delete the Job before applying (kubectl delete job migrate), give each version of the Job a new name, or make it a Helm hook so it is recreated on each upgrade
      ERROR ConfigMap shop-config (namespace shop), examples/shop/v2-breaking/app.yaml:89
            the ConfigMap is marked immutable: true and its data changed: kubectl apply and helm upgrade fail with "field is immutable when immutable is set"
            data.LOG_LEVEL changed
            fix: give the ConfigMap a new name for each version of its content (Kustomize's generators and many charts add a hash suffix) and point the workloads at it, or delete it and apply again
      ERROR RoleBinding shop-reader (namespace shop), examples/shop/v2-breaking/app.yaml:119
            roleRef changed, and a binding's roleRef can't be changed: kubectl apply and helm upgrade fail with "field is immutable"
            old: {"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"shop-reader"}
            new: {"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"view"}
            fix: delete the RoleBinding and apply again (its subjects lose these permissions until it is recreated), or create a binding with a new name
      WARN  PersistentVolumeClaim uploads (namespace shop), examples/shop/v2-breaking/app.yaml:98
            the storage request grew. This works only if the claim is bound and its StorageClass sets allowVolumeExpansion: true; otherwise the apply fails
            old: 5Gi
            new: 8Gi
            fix: check the StorageClass with kubectl get storageclass
      note  kinds with no immutable fields that wontpatch knows of were matched but not compared: Role (1), ServiceAccount (1)
      6 errors, 1 warning.

Applying `v1` and then `v2-breaking` to kube-apiserver v1.37.0 with `kubectl apply` rejects exactly those six objects, and the PersistentVolumeClaim too, because the claim on the test server is unbound, and an unbound claim can't change at all. `examples/shop/v2-safe` changes images, replicas, labels, ports and rules, which the API server accepts, and stays quiet:

    $ go run ./plugin/scripts examples/shop/v1 examples/shop/v2-safe
    wontpatch: OLD examples/shop/v1 (9 objects), NEW examples/shop/v2-safe (9 objects)
      9 matched, 0 only in NEW (created, nothing to compare), 0 only in OLD
      note  kinds with no immutable fields that wontpatch knows of were matched but not compared: Role (1), ServiceAccount (1)
      No immutable-field changes in the 9 objects present in both.

On real chart output, the prometheus-node-exporter chart changed its DaemonSet labels in 4.0.0, and its README tells you to delete the DaemonSet first. `helm upgrade` from 3.4.0 fails, and wontpatch finds it from the rendered files:

    $ go run ./plugin/scripts --namespace monitoring tests/testdata/charts/node-exporter-3.4.0.yaml tests/testdata/charts/node-exporter-4.0.0.yaml --fail-on none
    wontpatch: OLD tests/testdata/charts/node-exporter-3.4.0.yaml (3 objects), NEW tests/testdata/charts/node-exporter-4.0.0.yaml (3 objects)
      3 matched, 0 only in NEW (created, nothing to compare), 0 only in OLD
      ERROR DaemonSet demo-prometheus-node-exporter (namespace monitoring), tests/testdata/charts/node-exporter-4.0.0.yaml:48
            spec.selector changed, and a DaemonSet's selector can't be changed: kubectl apply and helm upgrade fail with "field is immutable"
            old: {"matchLabels":{"app":"prometheus-node-exporter","release":"demo"}}
            new: {"matchLabels":{"app.kubernetes.io/instance":"demo","app.kubernetes.io/name":"prometheus-node-exporter"}}
            matchLabels: added app.kubernetes.io/instance=demo, added app.kubernetes.io/name=prometheus-node-exporter, removed app, removed release
            fix: keep the old selector, or run kubectl delete daemonset demo-prometheus-node-exporter before applying, which stops its pods until the new ones start
      note  kinds with no immutable fields that wontpatch knows of were matched but not compared: ServiceAccount (1)
      1 error, 0 warnings.

Add `--format json` for machine-readable output.

## Getting the two sides

OLD is what the cluster has now and NEW is what you will apply. Render both the same way, with the same release name, namespace and values.

| You deploy with | OLD | NEW |
|---|---|---|
| Helm | `helm get manifest RELEASE -n NS` | `helm template RELEASE CHART -n NS -f values.yaml` |
| Helm, in a pull request | `helm template` at the base commit | `helm template` at the head commit |
| Kustomize, Argo CD or Flux | `kustomize build` at the base commit | `kustomize build` at the head commit |
| `kubectl apply -f` | the files at the base commit, or `kubectl get ... -o yaml` | the files you will apply |

Each side is a file, a directory of `.yaml`, `.yml` and `.json` files, or `-` for standard input. `helm template` usually leaves `metadata.namespace` out, so pass `--namespace NS` to match objects. After a failed upgrade, `helm get manifest` shows the failed revision: use `--revision` with the last revision `helm history` lists as deployed.

From a live cluster, `kubectl get deployment,statefulset -n NS -o yaml` works for objects applied with `kubectl apply`, because wontpatch reads their `kubectl.kubernetes.io/last-applied-configuration` annotation. Objects managed by Helm or server-side apply don't have it, so wontpatch refuses them when NEW has the same object, and you use `helm get manifest` or a render of the old revision instead. Live objects without it that aren't in NEW, like the `kube-root-ca.crt` ConfigMap every namespace gets, are left out with a note. I checked this path on objects applied with `kubectl apply` to the test API server and exported with `kubectl get -o yaml`.

In CI, for example on a pull request:

    git show origin/main:k8s/app.yaml > old.yaml
    go run github.com/GillMacedo/wontpatch/plugin/scripts@v0.1.0 old.yaml k8s/app.yaml

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Nothing at or above `--fail-on`, which is `warn` by default. Use `--fail-on error` to fail only on errors, or `none` to never fail on findings. |
| 1 | Findings at or above `--fail-on`. |
| 2 | The input can't be trusted, so nothing was compared and no result is printed. `--fail-on none` doesn't change this. See below. |

wontpatch exits 2, and prints only the reason, when either side is missing, empty, or a directory with no manifests; isn't valid YAML or JSON; holds something other than Kubernetes objects, such as an error message from a failed `helm template` or `kustomize build`, a values file or `helm install --dry-run` output; or an API error response (`kind: Status`); is a chart directory, a `Kustomization`, or a template with `{{ }}` left in a name; looks cut off (no final newline, a final `---` with nothing after it, or a workload missing fields the API server requires); has content on a `---` line, which kubectl refuses; has, in a directory, a file with no objects in it; defines the same object twice; or is a live object with no last-applied annotation that NEW also has. So a failed render can't pass CI as a clean result.

A file cut on a line break can still parse, and no tool can always tell it from a file that was edited. wontpatch reports each of these as a WARN, so they exit 1 under the default `--fail-on warn`: objects missing from NEW (`removed`); everything after OLD's last object being new (`after-end`); and a last object that ends on a key with no value, or that lines up with its counterpart and stops before it does (`cut-off`). The same warnings appear when you really remove objects or add lines at the end of the last object. Then they are fine, and `--fail-on error` ignores them.

## What it reports

Every ERROR below was checked against kube-apiserver v1.37.0 with both client-side and server-side `kubectl apply`, as described under "Tested on".

| Code | Severity | Objects and fields |
|---|---|---|
| `selector` | ERROR | `spec.selector` of a Deployment, ReplicaSet, DaemonSet or StatefulSet, including labels with empty values |
| `statefulset` | ERROR | StatefulSet `serviceName`, `podManagementPolicy` and `volumeClaimTemplates`, including their labels, size and storage class |
| `job` | ERROR | Job `spec.template`, `completions` (except an Indexed Job that changes `completions` and `parallelism` together), `completionMode`, `podFailurePolicy`, `successPolicy`, `backoffLimitPerIndex`, `managedBy` and `selector`. WARN instead when OLD sets `ttlSecondsAfterFinished`, because the finished Job may be gone already |
| `service` | ERROR | Service `clusterIP` changed from one address to another, or to `None`, the primary family in `ipFamilies`, and `loadBalancerClass` while the type stays `LoadBalancer`. WARN when NEW sets an address or family the cluster chose before |
| `pvc` | ERROR | PersistentVolumeClaim spec changes other than the storage request, and a smaller storage request. WARN for a bigger request, a new `storageClassName` and a new `volumeAttributesClassName`, which depend on the cluster |
| `pv` | ERROR | PersistentVolume volume source, `volumeMode`, and `nodeAffinity` once set |
| `roleref` | ERROR | `roleRef` of a RoleBinding or ClusterRoleBinding |
| `immutable` | ERROR | Data of a ConfigMap or Secret that OLD marks `immutable: true`, or turning that off. Secret values are never printed |
| `secret-type` | ERROR | Secret `type` |
| `storageclass` | ERROR | StorageClass `provisioner`, `parameters`, `reclaimPolicy` and `volumeBindingMode` |
| `priorityclass` | ERROR | PriorityClass `value` and `preemptionPolicy` |
| `ingressclass` | ERROR | IngressClass `spec.controller` |
| `csidriver` | ERROR | CSIDriver `attachRequired` and `volumeLifecycleModes` |
| `removed` | WARN | Objects in OLD and not in NEW, which `helm upgrade` and `kubectl apply --prune` delete |
| `after-end` | WARN | Everything after the last object of OLD's file is new in NEW: objects added at the end, or a cut-off OLD |
| `cut-off` | WARN | The last object of a file ends on a key with no value, or stops before its counterpart does: lines removed or added at the end, or a cut-off file |
| `hook` | note | Helm and Argo CD hooks, which are recreated rather than patched, so they aren't compared |
| `added`, `unchecked-kind`, `skipped` | note | New objects, matched kinds with no rules, objects with `generateName`, and live objects left out because NEW doesn't have them |

Defaults count as equal to unset: `podManagementPolicy: OrderedReady`, `volumeMode: Filesystem`, Secret type `Opaque`, `1Gi` and `1024Mi`, and the pod template defaults the API server fills in for a Job, such as `imagePullPolicy` (with the API server's rules for `latest`, digests and invalid names), `dnsPolicy`, probe timings and paths, `divisor`, token expiry and the `job-name` labels, so writing out a default isn't reported. Empty values follow the API server's types: `securityContext: {}` on a container or `affinity: {}` is a change, and so is a label with an empty value, but `args: []` and `labels: {}` are not.

## Limits

- **OLD has to be what the cluster has.** If someone changed an object by hand, or an operator or webhook rewrites it, the answer can differ. wontpatch never connects to a cluster.
- **Only the kinds in the table.** Custom resources have their own rules, often in CEL validations, and aren't checked. Bare Pods aren't checked either.
- **Apply semantics beyond immutability are out of scope.** For example, switching a Deployment's strategy to `Recreate` fails under server-side apply because the old `rollingUpdate` block stays. wontpatch doesn't report that.
- **Moving from client-side to server-side apply** keeps fields that were removed from the manifest, so a removal wontpatch reports as an ERROR may not fail on that one apply.
- **A suspended Job that never started** accepts some template changes. wontpatch notes that case and can't know whether the Job started.
- **Namespaces are matched as written.** Use `--namespace` when the renders leave them out.

## FAQ

**Why does helm upgrade fail with "field is immutable"?** The new chart changes a field Kubernetes doesn't allow to change on an existing object, most often a Deployment, DaemonSet or StatefulSet selector, a StatefulSet's volumeClaimTemplates, or a Job's pod template. The object has to be deleted and recreated, or the old value kept. wontpatch names the object and the field before you upgrade.

**How do I change a StatefulSet's volumeClaimTemplates?** You can't in place. Run `kubectl delete statefulset NAME --cascade=orphan`, which keeps the pods and their volume claims, then apply. The new StatefulSet adopts the pods. Existing claims keep their old size, so resize each claim if the StorageClass allows expansion.

**Why does a Job fail to update when I change its image?** A Job's pod template can't change after creation. Delete the Job before applying, give each version a new name, or make it a Helm hook, which Helm recreates on each upgrade.

**Doesn't kubectl diff catch this?** `kubectl diff` and `helm upgrade --dry-run=server` ask the live API server, so they catch it if CI can reach the cluster with enough rights. wontpatch works on files alone, so it runs on a pull request with no cluster access.

**Doesn't helm-diff or kubeconform catch this?** helm-diff shows what changes but doesn't say which changes will be rejected. kubeconform checks each object against its schema, which doesn't know about the previous version.

**Is it an MCP server?** No. It's a skill with one Go program, so `go run` or `go build` is all it needs.

## Tested on

- **Against a real API server:** 155 cases in `tests/cases`, each an old and a new manifest, were applied to kube-apiserver v1.37.0 (the envtest build, with etcd and no controllers) with both client-side and server-side `kubectl apply`. For all 78 that wontpatch reports as ERROR, the server rejected the apply, and it accepted all 72 that wontpatch reports as fine. The 5 WARN cases depend on the cluster. `go test ./tests -run GroundTruth` repeats this when `scripts/apiserver.sh` has started a server, and CI runs it on every push. This testing found two pod template defaults that an early version assumed wrongly, `enableServiceLinks` and requests copied from limits. A separate review that tried to break wontpatch then found more, each confirmed on the same server: changes it missed because it treated empty values as unset, kinds and fields it didn't check, defaults it didn't fill in, a wrong pull policy rule, and input it should have refused. These are fixed, and their 31 cases are in `tests/cases` as `review-*`. Two findings were kept as they are: documents after a `...` end marker are ignored, as kubectl ignores them, and defaults of removed in-tree volume types aren't filled in.
- **Real charts with real Helm:** I ran `scripts/chart-sweep.sh` with Helm v3.19.0 over 62 pairs of consecutive versions of five prometheus-community charts (prometheus, alertmanager, kube-state-metrics, prometheus-node-exporter and prometheus-pushgateway). Each pair was installed and upgraded on the same API server. The 12 upgrades `helm upgrade` rejected are the 12 wontpatch reported as errors, among them the breaks the charts' upgrade notes document for node-exporter 4.0.0, prometheus 15.0.0 and pushgateway before 3.0.0. The other 50 upgraded and stayed quiet. Four of these renders are kept in `tests/testdata/charts` as tests.
- **Untrusted input:** tests feed each of these as OLD and as NEW, with the other side real, under every `--fail-on` and `--format`, and check exit 2 with nothing on standard output: empty and whitespace files, comments only, binary data, random text, error output from failed `helm`, `kustomize`, `kubectl` and `go build` runs, `helm install --dry-run` output, a values file, invalid YAML, truncated and trailing-garbage JSON, objects without name or metadata, workloads without spec or containers, duplicates, live objects without the annotation, bad annotations, unrendered templates, a chart directory, a Kustomization, content on a `---` line, a saved API error (`kind: Status`), and a directory with a file that holds only `---`, `null`, comments or an empty List. Other tests cut the example at every byte, as OLD and as NEW, against the same version and a different one, and cut a changed Deployment on every line break, and check that no cut passes as clean: each exits 2, or exits 1 with a warning.
- **The installed plugin:** a test copies `plugin/` outside the repository and builds it with the skill's own command.
- **Go versions:** tests pass on Go 1.22.12 and Go 1.27.3 on Linux arm64. CI runs Linux amd64.

Not tested: in-tree volume types that Kubernetes removed (rbd, scaleIO, azureDisk and others), whose defaults wontpatch doesn't fill in; other Kubernetes versions, which may make more fields mutable over time; managed clusters, admission webhooks and real controllers; Argo CD and Flux themselves; Windows and macOS; and a fresh Claude Code session using the skill. The `go run ...@v0.1.0` lines work only once the repository is published.

## Contributing

Contributions are welcome, from people and from people working with AI agents. See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are [listed here](https://github.com/GillMacedo/wontpatch/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

### How pull requests work

1. **Open a PR.** Fork the repo, work on a branch, and keep it to one change. Write a title that says what changes for users, because it becomes the changelog line: `feat:` goes under Added and `fix:` under Fixed.
2. **Automatic check.** Code changes need tests, and `make test` must pass. Changes over 30 lines need a linked open issue (`Closes #<number>`), and a PR can have at most 800 changed lines. If something is missing, a comment tells you what to fix.
3. **Review.** Claude reviews each PR automatically. Code PRs also get a security review and are tested in a sandbox. The review then merges the PR or asks for changes, and a small problem may become a follow-up issue instead. A PR with no new commit 14 days after a change request is closed, and you can reopen it. PRs that change CI, agent settings, npm or Python dependencies, vendored code or build files can't be merged this way, and a comment says so. The same goes for anything the security review doesn't clear.
4. **Release.** A merge that changes what users install becomes a new version within about two hours, with a GitHub Release and, if the project has one, its npm package. Docs and CI changes wait for the next release.

## AI disclosure

This project was largely generated with Claude Code and reviewed by its maintainer.

## About me

I'm Giulliano Macedo, and I build developer tools at GoTech Solutions. Follow along or get in touch:

- GitHub: [GillMacedo](https://github.com/GillMacedo)
- LinkedIn: [giulliano](https://www.linkedin.com/in/giulliano/)
- X: [@giumacedo](https://x.com/giumacedo)

If `wontpatch` saved you some prompts, a star helps others find it.

---

Built by [Giulliano Macedo](https://github.com/GillMacedo), GoTech Solutions · [LinkedIn](https://www.linkedin.com/in/giulliano/) · [X](https://x.com/giumacedo)
