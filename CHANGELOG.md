# Changelog

All notable changes are documented here, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. This project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-08

### Added
- Compares two sets of rendered Kubernetes manifests and reports changes the API server rejects as immutable: workload selectors, StatefulSet `serviceName`, `podManagementPolicy` and `volumeClaimTemplates`, Job pod templates and fields, Service `clusterIP` and `ipFamilies`, PersistentVolumeClaim and PersistentVolume specs, RoleBinding `roleRef`, immutable ConfigMaps and Secrets, Secret `type`, StorageClass, PriorityClass, IngressClass and CSIDriver fields.
- Warnings for removed objects, PVC changes that depend on the cluster, Jobs with `ttlSecondsAfterFinished`, new objects after the end of OLD's file, and files that look cut off on a line break.
- Reads live objects from `kubectl get -o yaml` through the last-applied annotation.
- Exit 2 with no result for input it can't trust: empty, failed or cut-off renders, charts, kustomizations, unrendered templates and duplicates.
- Text and JSON output, `--fail-on warn|error|none` and `--namespace`.
- `checking-immutable-fields` skill and plugin manifest.
