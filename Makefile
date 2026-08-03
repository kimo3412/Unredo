.PHONY: tidy build test test-integration doctor txn-list seed unredo fmt vet run clean-plans release-check

GO ?= go
VERSION ?= 0.1.0-dev
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X github.com/girimi/unredo/internal/buildinfo.Version=$(VERSION) -X github.com/girimi/unredo/internal/buildinfo.Commit=$(COMMIT) -X github.com/girimi/unredo/internal/buildinfo.BuildDate=$(BUILD_DATE)

tidy:
	$(GO) mod tidy

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/unredo.exe ./cmd/unredo

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...

test:
	$(GO) test ./...

test-integration: build
	$(GO) test -tags=integration -timeout 300s ./...

release-check: test vet build
	./bin/unredo.exe version

run: build
	./bin/unredo.exe --config unredo.yaml --profile local

doctor: build
	./bin/unredo.exe --config unredo.yaml --profile local doctor

txn-list: build
	./bin/unredo.exe --config unredo.yaml --profile local txn list --limit 10

seed:
	mysql -uunredo_executor -punredo_executor_pw -h 127.0.0.1 --protocol=TCP unredo_shop < scripts/seed_fixtures.sql

unredo: build
	./bin/unredo.exe --config unredo.yaml --profile local

clean-plans:
	rm -f testdata/plans/*.json
