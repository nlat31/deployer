.PHONY: build test sample
build:
	./build.sh
test:
	npm --prefix web run build
	go test -race ./...
	go vet ./...
sample:
	./scripts/package-sample.sh 1.0.0

.PHONY: test-lifecycle test-update
test-lifecycle:
	go build -o dist/deployer-launcher ./cmd/deployer-launcher
	unshare -Ur python3 scripts/test-lifecycle.py
test-update:
	unshare -Ur python3 scripts/test-auto-update.py
