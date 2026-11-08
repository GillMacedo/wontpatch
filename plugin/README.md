# wontpatch plugin

This plugin adds the `checking-immutable-fields` skill to Claude Code. The skill runs `scripts/wontpatch.go` on two sets of rendered Kubernetes manifests, the one deployed now and the one about to be applied, and reports changes the API server rejects because the field can't change after creation. It needs Go 1.22 or newer. It reads files only and never connects to a cluster.

See the repository README for examples, exit codes and limits: https://github.com/GillMacedo/wontpatch

## Install

    claude plugin marketplace add GillMacedo/wontpatch
    claude plugin install wontpatch@wontpatch-marketplace
