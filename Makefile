.PHONY: build install clean release test-go test-web test e2e test-all

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS = -s -w -X main.version=$(VERSION)

build:
	cd cli && CGO_ENABLED=0 go build -o uplink -ldflags="$(LDFLAGS)"

install: build
	install -Dm755 cli/uplink /usr/local/bin/uplink

clean:
	rm -f cli/uplink
	rm -rf cli/build

test-go:
	cd cli && go test -race ./...
	cd server && go test -race ./...

test-web:
	npx tsx tests/web/run_tests.ts
	npx vitest run

test: test-go test-web

# E2E needs a live server and a built CLI (cli/build/uplink); see tests/README.md first.
export SERVER ?= http://localhost:3000

e2e:
	./tests/e2e/e2e_phase0.sh
	./tests/e2e/session_flow_test.sh
	./tests/e2e/chat_two_clients.sh

test-all: test e2e

release:
	mkdir -p cli/build
	# Linux AMD64
	cd cli && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -buildmode=pie -o build/uplink -ldflags="$(LDFLAGS)"
	cd cli/build && tar -czf uplink-linux-amd64.tar.gz uplink && rm uplink
	# Linux ARM64
	cd cli && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -buildmode=pie -o build/uplink -ldflags="$(LDFLAGS)"
	cd cli/build && tar -czf uplink-linux-arm64.tar.gz uplink && rm uplink
	# Windows AMD64
	cd cli && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o build/uplink.exe -ldflags="$(LDFLAGS)"
	cd cli/build && tar -czf uplink-windows-amd64.tar.gz uplink.exe && rm uplink.exe
	# Darwin AMD64
	cd cli && GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -o build/uplink -ldflags="$(LDFLAGS)"
	cd cli/build && tar -czf uplink-darwin-amd64.tar.gz uplink && rm uplink
	# Darwin ARM64
	cd cli && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o build/uplink -ldflags="$(LDFLAGS)"
	cd cli/build && tar -czf uplink-darwin-arm64.tar.gz uplink && rm uplink
