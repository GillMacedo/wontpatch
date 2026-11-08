# 0003: Check every rule against a real API server

**Decision:** each rule has old and new manifests in `tests/cases` with the expected outcome, and CI applies all of them to kube-apiserver from the envtest release with client-side and server-side apply.

**Why:** an ERROR claims that the apply will fail, so it has to be checked against the thing that rejects it. Reading the validation code alone got two pod template defaults wrong, which the server caught. envtest runs etcd and kube-apiserver without Docker or a cloud account, so the check runs on a laptop and in CI.

**Limit:** the server has no controllers, so behavior that depends on them, such as a bound PVC or a started Job, is set up by hand or reported as WARN. Only one Kubernetes version is checked.
