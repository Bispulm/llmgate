# Common development tasks. `make check` is what must pass before a commit.
.PHONY: build test vet lint vuln secrets check demo

build:
	go build ./...

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

vuln:
	govulncheck ./...

secrets:
	gitleaks dir . --no-banner --redact --exit-code 1
	gitleaks git . --no-banner --redact --exit-code 1

check: build vet test lint vuln secrets

demo:
	./scripts/demo.sh
