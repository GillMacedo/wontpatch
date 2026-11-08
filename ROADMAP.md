# Roadmap

## Next
- CronJob `jobTemplate` changes that only take effect for the next Job, as a note
- Deployment and StatefulSet strategy switches that fail under server-side apply because the old `rollingUpdate` block stays
- Argo CD `Replace=true` and `Force=true` sync options, once they can be tested against Argo CD
- A `--kube-version` flag for rules that differ between Kubernetes versions

## Later
- Custom resources whose CRDs mark fields immutable with CEL rules like `self == oldSelf`, read from the CRD in OLD
- A GitHub Action that renders both sides of a pull request and comments the findings
- Bare Pods
- Defaults of removed in-tree volume types (rbd, scaleIO, azureDisk)
