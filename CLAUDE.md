# wontpatch

Claude Code skill and Go CLI that compares two sets of rendered Kubernetes manifests, OLD (deployed) and NEW (about to be applied), and reports changes the API server rejects because the field can't change after creation.

## Architecture
`plugin/scripts/wontpatch.go` holds the logic. `loadSide` and `parseFile` read files, split YAML documents the way kubectl does and refuse input they can't trust. `liveWithoutAnnotation` refuses more of it once both sides are loaded, and `cutOff` and `appendedAfterEnd` warn about files that may be cut off on a line break. `compare` matches objects by kind, namespace and name, and `checkPair` runs the rules for each kind. `normPodTemplate` and `canon` fill in API server defaults and canonical numbers and quantities before comparing. `plugin/scripts/yaml_*.go` is go.yaml.in/yaml/v2 copied in, so the plugin builds with no go.mod. `plugin/skills/checking-immutable-fields/SKILL.md` tells Claude how to use it. See `docs/architecture.md`.

## Commands
- `make test`: gofmt, vet and tests, including `tests/`
- `make groundtruth`: starts a local kube-apiserver with `scripts/apiserver.sh` and checks every case in `tests/cases` against it (Linux, needs network once)
- `make lint`: plugin manifest validation
- `make demo`: run on the examples

## Conventions
- Standard library only, plus the copied YAML parser. Never connect to a cluster.
- Any input it can't fully read or trust exits 2 with no result printed, whatever `--fail-on` says. Add a case to `garbage` in `plugin/scripts/wontpatch_test.go` for each new kind of input.
- An ERROR must mean the API server rejects the apply. Every rule needs cases in `tests/cases` for what it flags and for what must stay quiet, checked with `make groundtruth`. If the outcome depends on the cluster, it's a WARN.
- Never print Secret or ConfigMap values.

## Do and don't
- Do run `make groundtruth` before calling a rule change done.
- Do keep `tests/examples_test.go` and `tests/charts_test.go` passing.
- Don't add `go.mod` requirements: the installed plugin has no module.
- Don't print "No immutable-field changes" for a run that compared nothing it claims to have compared.

## Definition of done
`make test`, `make groundtruth` and `make lint` pass, README output matches a real run, and `ROADMAP.md` and docs are updated when relevant.
