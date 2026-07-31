.PHONY: tidy build test doctor txn-list seed unredo fmt vet run

GO ?= go

tidy:
	$(GO) mod tidy

build:
	$(GO) build -o bin/unredo.exe ./cmd/unredo

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

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
