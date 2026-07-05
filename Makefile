BINARY  := stallwatch
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build linux test lint clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/stallwatch

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-amd64 ./cmd/stallwatch

test:
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...

clean:
	rm -f $(BINARY) $(BINARY)-linux-amd64
