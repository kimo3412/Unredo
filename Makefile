.PHONY: tidy build test test-integration doctor txn-list seed unredo fmt vet run clean-plans

GO ?= go

tidy:
	$(GO) mod tidy

build:
	$(GO) build -o bin/unredo.exe ./cmd/unredo

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...

test:
	$(GO) test ./...

test-integration: build
	$(GO) test -tags=integration -timeout 60s ./...

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

