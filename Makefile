BINARY := nextcloud-sync-daemon
VERSION ?= dev
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test lint clean

build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/nextcloud-sync-daemon/

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	go vet ./...
	@which golangci-lint > /dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed, skipping"

clean:
	rm -f $(BINARY) coverage.out
