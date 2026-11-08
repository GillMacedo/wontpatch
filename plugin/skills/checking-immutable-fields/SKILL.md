---
name: checking-immutable-fields
description: Compares two sets of rendered Kubernetes manifests, what is deployed now and what is about to be applied, and reports changes the API server rejects because the field can't change after creation, such as a Deployment, DaemonSet or StatefulSet selector, StatefulSet volumeClaimTemplates, serviceName or podManagementPolicy, a Job's pod template, a RoleBinding roleRef, a Service clusterIP or loadBalancerClass, a PVC spec, an immutable ConfigMap or Secret, a StorageClass, PriorityClass, IngressClass or CSIDriver. Use before helm upgrade, kubectl apply or an Argo CD or Flux sync, when reviewing a chart or manifest change, or when an upgrade failed with "field is immutable", "cannot patch" or "spec is immutable after creation".
---

# Checking Kubernetes immutable fields

The tool compares files. It never talks to a cluster. It needs Go 1.22 or newer.

## Get the two sides

OLD is what the cluster has now and NEW is what will be applied. Pick the pair that fits the project, and render both sides the same way, with the same release name, namespace and values:

- Helm release: `helm get manifest RELEASE -n NS > old.yaml` and `helm template RELEASE CHART -n NS -f values.yaml > new.yaml`. After a failed upgrade, `helm get manifest` shows the failed revision, so pass `--revision` with the last revision that `helm history` lists as deployed.
- Chart change in a pull request: `helm template` at the base commit and at the head commit.
- Kustomize: `kustomize build` (or `kubectl kustomize`) at both commits.
- Plain manifests applied with `kubectl apply`: the files at both commits, or `kubectl get KIND -n NS -o yaml` for OLD. wontpatch reads the last-applied annotation from live objects and refuses live objects without it.

Never pass a chart directory or a kustomization directory. They aren't rendered, and the tool refuses them.

## Run it

Build it once, then run it on the two sides:

    go build -o "${TMPDIR:-/tmp}/wontpatch" $(ls "${CLAUDE_PLUGIN_ROOT}"/scripts/*.go | grep -v _test.go)
    "${TMPDIR:-/tmp}/wontpatch" OLD NEW

Each side is a file or a directory. Use `--namespace NS` when the rendered objects don't set a namespace (helm template output usually doesn't), and `--format json` for machine-readable output.

## Read the exit code first

- **0:** nothing at or above `--fail-on` (default `warn`). Read the notes anyway.
- **1:** errors or warnings.
- **2:** the input can't be trusted, so nothing was compared. The message says why: an empty file, a failed render, a file that looks cut off, a live object without the last-applied annotation, a chart or kustomization directory. Tell the user, fix the input and run again. Never describe exit 2 as a clean result.

## Check each finding

- **ERROR** means the API server rejects the apply for that object, given that OLD is what the cluster has. Quote the field, the old and new values, and the fix line.
- **WARN** means it depends on the cluster or on what really happened: a PVC that grows (needs a bound claim and an expandable StorageClass), a Job with `ttlSecondsAfterFinished` that may already be gone, a Service IP or IP family that the cluster picked, an object removed from NEW, new objects after the end of OLD's file, or a last object that looks cut off (`cut-off`). The last two are also what a file cut on a line break looks like, so check that both renders finished before calling them fine.
- **note** lists hooks, generated names, new objects and kinds that weren't compared.

## Report

1. Errors first, each with the object, the field, and the smallest fix: keep the old value, or delete the object first. For a StatefulSet, `kubectl delete statefulset NAME --cascade=orphan` keeps its pods and volumes. Deleting a Service gives it a new IP. Deleting a RoleBinding drops its permissions until it is recreated.
2. Then warnings, and whether each matters here.
3. Say what wasn't checked: the notes, and kinds the tool has no rules for, such as custom resources.

## Don't

Don't delete or change anything in the cluster unless the user asks, and say what a delete does to running pods, IPs and permissions before suggesting it. Don't say an upgrade is safe because wontpatch found nothing: it only knows about immutable fields, not about whether the new version works.
