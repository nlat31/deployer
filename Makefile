.PHONY: build test sample
build:
	./build.sh
test:
	npm --prefix web run build
	go test -race ./...
	go vet ./...
sample:
	./scripts/package-sample.sh 1.0.0
