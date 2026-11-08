package tests

import (
	"reflect"
	"testing"
)

// TestRealCharts runs wontpatch on real helm template output from
// prometheus-community charts. Each failing upgrade was confirmed with a real
// helm upgrade against kube-apiserver v1.37.0, and each passing one upgraded.
func TestRealCharts(t *testing.T) {
	for _, tc := range []struct {
		old, new string
		errors   []string
	}{
		{"node-exporter-3.4.0", "node-exporter-4.0.0", []string{"DaemonSet demo-prometheus-node-exporter"}},
		{"node-exporter-4.0.0", "node-exporter-4.1.0", nil},
		{"pushgateway-2.14.0", "pushgateway-2.15.0", []string{"StatefulSet demo-prometheus-pushgateway"}},
		{"pushgateway-3.0.0", "pushgateway-3.1.0", nil},
	} {
		rep, r := runJSON(t, "--namespace", "monitoring", "testdata/charts/"+tc.old+".yaml", "testdata/charts/"+tc.new+".yaml")
		if got := objectsWith(rep, "ERROR"); !reflect.DeepEqual(got, tc.errors) {
			t.Errorf("%s -> %s: errors on %v, want %v\n%s", tc.old, tc.new, got, tc.errors, r.stdout)
		}
		if got := objectsWith(rep, "WARN"); len(got) != 0 {
			t.Errorf("%s -> %s: unexpected warnings on %v", tc.old, tc.new, got)
		}
		if rep.Matched < 3 {
			t.Errorf("%s -> %s: only %d objects matched", tc.old, tc.new, rep.Matched)
		}
	}
}
