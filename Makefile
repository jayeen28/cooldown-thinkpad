VERSION ?= 1.0.0
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath

.PHONY: all test linux windows clean

all: test linux windows

test:
	go vet ./...
	GOOS=windows go vet ./...
	go test ./...

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/cooldown-linux-amd64 .

windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/cooldown-windows-amd64.exe .

clean:
	rm -rf dist
