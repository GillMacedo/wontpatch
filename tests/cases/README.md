# Cases

Each directory holds `old.json`, `new.json` and `want`:

- `error`: wontpatch reports an ERROR, and kube-apiserver rejects applying `new.json` over `old.json`
- `ok`: wontpatch reports no ERROR or WARN, and the apply succeeds
- `warn`: wontpatch reports a WARN and no ERROR, because the outcome depends on the cluster

`{{name}}` is the name of a cluster-scoped object and `{{ipN}}` a Service cluster IP. The tests fill them in, with fresh values for each run against an API server.

`go test ./tests -run TestCases` checks wontpatch's answers. `make groundtruth` also applies each case to a local API server with client-side and server-side apply.
