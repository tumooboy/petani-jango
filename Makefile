BIN := bin/jango-bulk

.PHONY: build test vet fmt run clean

build:
	go build -trimpath -ldflags "-s -w" -o $(BIN) ./cmd/jango-bulk

test:
	go test ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -l -w internal cmd

clean:
	rm -rf bin
