.PHONY: test test-unit test-regtest build up down fmt vet

build:
	go build -o bin/paygate ./cmd/paygate

test: test-unit

test-unit:
	go test ./...

# Integration tests against a real bitcoind. Kept behind a build tag so
# `go test ./...` stays fast and needs nothing installed.
#
# The chain is wiped first. A shared, accumulating regtest node is how
# integration suites become flaky: run two of them and the second sees the
# first one's payments.
test-regtest: reset
	go test -tags=regtest -count=1 -v ./test/regtest/

reset: down up

up:
	docker compose up -d --wait

down:
	docker compose down -v

fmt:
	gofmt -l -w .

vet:
	go vet ./...
