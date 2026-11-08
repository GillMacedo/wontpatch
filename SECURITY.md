# Security

Report vulnerabilities through [GitHub's private vulnerability reporting](https://github.com/GillMacedo/wontpatch/security/advisories/new), not in a public issue.

`wontpatch` reads files only. It never connects to a cluster and never runs anything from its input. Manifests can hold secrets, so it prints the names of changed keys in ConfigMaps and Secrets and never their values.

A missed or wrong finding is a normal bug. If `wontpatch` prints a Secret value, or reports a clean result for input it couldn't read, report it privately.
