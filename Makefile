BINARY := plum
LDFLAGS := -s -w -X main.version=$(shell git describe --tags --always 2>/dev/null || echo dev)

.PHONY: build cross test golden shims lint install clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/plum

# CGO_ENABLED=0 is the acceptance test for the toolchain decision: if this ever
# fails, a cgo dependency has crept in and cross-compilation is gone.
cross:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64 ./cmd/plum
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-amd64 ./cmd/plum
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64  ./cmd/plum
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64  ./cmd/plum
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe ./cmd/plum

test:
	go test ./... -race

golden:
	go test ./internal/extract -run Golden -update

# Shims are embedded from internal/trace/shim_assets because go:embed cannot
# reach outside its package. Edit the readable copy under shims/, then run this.
shims:
	cp shims/python/plum_shim.py shims/python/sitecustomize.py shims/node/plum-shim.cjs internal/trace/shim_assets/

lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...

install: build
	install -m 0755 bin/$(BINARY) $(or $(PREFIX),$(HOME)/.local)/bin/$(BINARY)

clean:
	rm -rf bin dist
