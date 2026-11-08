#!/bin/sh
# Downloads the Kubernetes envtest binaries (etcd, kube-apiserver, kubectl),
# checks their sha512, and starts a local API server with no controllers.
# The ground-truth tests apply each case to it. Linux only.
#
#   eval "$(scripts/apiserver.sh /tmp/wontpatch-apiserver)"
#   go test ./tests -run GroundTruth -v
#
# It prints the WONTPATCH_KUBECTL and KUBECONFIG exports for the tests.
set -eu
dir=${1:-/tmp/wontpatch-apiserver}
version=v1.37.0
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64; sum=1d1c453633b72c161a5d5a886cde7ac850be1a2ac796a9e1d4ffacacc64868295bdd2d57aa66cd0c158f5ce510f5dfe3fbc61ac21bd3dcfb875bd70658aa663a ;;
  aarch64 | arm64) arch=arm64; sum=ae6a670502988200b0131c943758cfd3d3a58cf4e6247ef7f0e6a6467f1cd9a333c802e86101f0446b1b92a0e327387224e1e44fe1a4693da0929c6e529cfe9a ;;
  *) echo "apiserver.sh: no envtest build for $(uname -m)" >&2; exit 1 ;;
esac
mkdir -p "$dir"
tarball="$dir/envtest-$version-linux-$arch.tar.gz"
if [ ! -f "$tarball" ]; then
  curl -fsSL -o "$tarball" "https://github.com/kubernetes-sigs/controller-tools/releases/download/envtest-$version/envtest-$version-linux-$arch.tar.gz"
fi
echo "$sum  $tarball" | sha512sum -c - >&2
tar -xzf "$tarball" -C "$dir"
bin="$dir/controller-tools/envtest"

openssl genrsa -out "$dir/sa.key" 2048 2>/dev/null
openssl rsa -in "$dir/sa.key" -pubout -out "$dir/sa.pub" 2>/dev/null
echo "wontpatch-test-token,admin,admin,system:masters" > "$dir/tokens.csv"

"$bin/etcd" --data-dir "$dir/etcd" \
  --listen-client-urls http://127.0.0.1:12379 --advertise-client-urls http://127.0.0.1:12379 \
  --listen-peer-urls http://127.0.0.1:12380 > "$dir/etcd.log" 2>&1 &
"$bin/kube-apiserver" --etcd-servers http://127.0.0.1:12379 --cert-dir "$dir/certs" \
  --secure-port 16443 --bind-address 127.0.0.1 \
  --service-account-key-file "$dir/sa.pub" --service-account-signing-key-file "$dir/sa.key" \
  --service-account-issuer https://kubernetes.default.svc \
  --token-auth-file "$dir/tokens.csv" --authorization-mode AlwaysAllow \
  --service-cluster-ip-range 10.0.0.0/16 --disable-admission-plugins ServiceAccount > "$dir/apiserver.log" 2>&1 &

cat > "$dir/kubeconfig" <<KUBECONFIG
apiVersion: v1
kind: Config
clusters: [{name: local, cluster: {server: "https://127.0.0.1:16443", insecure-skip-tls-verify: true}}]
users: [{name: admin, user: {token: wontpatch-test-token}}]
contexts: [{name: local, context: {cluster: local, user: admin}}]
current-context: local
KUBECONFIG

i=0
until "$bin/kubectl" --kubeconfig "$dir/kubeconfig" get --raw /readyz > /dev/null 2>&1; do
  i=$((i + 1))
  if [ $i -gt 60 ]; then
    echo "apiserver.sh: the API server didn't become ready; see $dir/apiserver.log" >&2
    exit 1
  fi
  sleep 1
done
echo "export WONTPATCH_KUBECTL=$bin/kubectl"
echo "export KUBECONFIG=$dir/kubeconfig"
