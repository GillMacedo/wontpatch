#!/bin/bash
# Compares wontpatch with real helm upgrades, one pair of consecutive chart
# versions at a time: install the older version, render both, run wontpatch,
# then upgrade and record whether the API server accepted it.
#
#   scripts/chart-sweep.sh REPO/CHART COUNT [SKIP] [helm flags...]
#   scripts/chart-sweep.sh prometheus-community/prometheus-node-exporter 6 107 --set rbac.pspEnabled=false
#
# It needs helm, a repo added with helm repo add, KUBECONFIG pointing at a
# test API server (scripts/apiserver.sh starts one), and Go. It installs and
# uninstalls releases, so never point it at a real cluster.
set -euo pipefail
chart=$1 count=$2 skip=${3:-0}
shift $(( $# < 3 ? $# : 3 ))
out=$(mktemp -d)
go build -o "$out/wontpatch" ./plugin/scripts
versions=$(helm search repo "$chart" --versions | awk -v c="$chart" '$1 == c {print $2}' | grep -v -- - | sed -n "$((skip + 1)),$((skip + count + 1))p" | tac)
prev=""
for v in $versions; do
  if [ -n "$prev" ]; then
    rel=sweep$RANDOM$RANDOM
    d="$out/$prev-$v"
    mkdir -p "$d"
    helm template "$rel" "$chart" --version "$prev" -n "$rel" "$@" > "$d/old.yaml"
    helm template "$rel" "$chart" --version "$v" -n "$rel" "$@" > "$d/new.yaml"
    set +e
    "$out/wontpatch" --namespace "$rel" --fail-on error "$d/old.yaml" "$d/new.yaml" > "$d/wontpatch.txt" 2>&1
    code=$?
    if helm install "$rel" "$chart" --version "$prev" -n "$rel" --create-namespace "$@" > "$d/install.log" 2>&1; then
      if helm upgrade "$rel" "$chart" --version "$v" -n "$rel" "$@" > "$d/upgrade.log" 2>&1; then helm_result=accepted; else helm_result=rejected; fi
    else
      helm_result="install failed"
    fi
    helm uninstall "$rel" -n "$rel" > /dev/null 2>&1
    set -e
    case $code in 0) verdict=ok ;; 1) verdict=errors ;; *) verdict="exit $code" ;; esac
    agree=""
    if { [ "$verdict" = ok ] && [ "$helm_result" = rejected ]; } || { [ "$verdict" = errors ] && [ "$helm_result" = accepted ]; }; then agree="  <- DISAGREE"; fi
    echo "$chart $prev -> $v: wontpatch $verdict, helm upgrade $helm_result$agree"
  fi
  prev=$v
done
echo "outputs in $out"
