# Rendered charts

These files are `helm template` output of two charts from
[prometheus-community/helm-charts](https://github.com/prometheus-community/helm-charts),
which is licensed under the Apache License 2.0. They are kept unchanged as test
data. Each was rendered with Helm v3.19.0, release name `demo` and namespace
`monitoring`:

    helm template demo prometheus-community/prometheus-node-exporter --version VERSION -n monitoring --set rbac.pspEnabled=false
    helm template demo prometheus-community/prometheus-pushgateway --version VERSION -n monitoring --set runAsStatefulSet=true --set persistentVolume.enabled=true

The chart READMEs document the breaks these tests expect: node-exporter 4.0.0
changed the DaemonSet's labels and asks you to delete the DaemonSet first, and
pushgateway before 3.0.0 put version labels on the StatefulSet's
volumeClaimTemplates, so every upgrade with persistence failed.
