.PHONY: build test run-server run-agent fmt vet

build:
	./scripts/build.sh

test:
	./scripts/test.sh

run-server: build
	./bin/fleet-server $(ARGS)

run-agent: build
	./bin/fleet-agent $(ARGS)

fmt:
	gofmt -w .

vet:
	go vet ./...
