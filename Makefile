# Common development tasks. `make check` is what must pass before a commit.

LLMGATE_POSTGRES_PORT ?= 15433
LLMGATE_REDIS_PORT ?= 16383
export LLMGATE_POSTGRES_PORT
export LLMGATE_REDIS_PORT
TEST_POSTGRES_URL = postgres://llmgate:llmgate_dev@127.0.0.1:$(LLMGATE_POSTGRES_PORT)/llmgate?sslmode=disable
TEST_REDIS_URL = redis://127.0.0.1:$(LLMGATE_REDIS_PORT)/0

.PHONY: build test vet lint vuln secrets check demo dev-up dev-down test-integration

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

dev-up:
	docker compose up -d --wait

dev-down:
	docker compose down

# Runs every test, including those needing real Postgres and Redis. It fails rather
# than skips if they can't be reached.
test-integration: dev-up
	LLMGATE_TEST_POSTGRES_URL='$(TEST_POSTGRES_URL)' LLMGATE_TEST_REDIS_URL='$(TEST_REDIS_URL)' LLMGATE_REQUIRE_INTEGRATION=1 \
		go test -race -count=1 -p 2 ./...
