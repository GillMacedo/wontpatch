.PHONY: test groundtruth lint demo

test:
	test -z "$$(gofmt -l plugin tests)"
	go vet ./...
	go test -race ./...

# Starts etcd and kube-apiserver from the envtest release (checked by sha512)
# and applies every case in tests/cases with client-side and server-side apply.
groundtruth:
	env="$$(scripts/apiserver.sh $${APISERVER_DIR:-/tmp/wontpatch-apiserver})" && eval "$$env" && \
		test -n "$$WONTPATCH_KUBECTL" && go test ./tests -run 'GroundTruth' -count=1 -v

lint:
	claude plugin validate ./plugin --strict
	claude plugin validate . --strict

# Exit 1 means it found problems, which the breaking example has on purpose.
demo:
	go run ./plugin/scripts examples/shop/v1 examples/shop/v2-breaking || test $$? -eq 1
	go run ./plugin/scripts examples/shop/v1 examples/shop/v2-safe
