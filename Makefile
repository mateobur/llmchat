# llmchat — build, test and cross-compile.
#
#   make            build ./llmchat
#   make run        build and start it (make run ADDR=:8090)
#   make check      what CI should run: formatting, vet, tests with -race
#   make dist       static binaries for every platform in PLATFORMS
#   make help       list every target

BINARY  := llmchat
GO      ?= go
GOFLAGS ?=
ADDR    ?= :8080
DIST    := dist

PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

# The web client is compiled into the binary with //go:embed, so changes under
# web/ have to trigger a rebuild just like changes to the Go files.
SOURCES := $(wildcard *.go) $(wildcard web/*) go.mod go.sum

.DEFAULT_GOAL := build
.PHONY: build run test race vet fmt check tidy dist clean help

build: $(BINARY)

$(BINARY): $(SOURCES)
	$(GO) build $(GOFLAGS) -o $@ .

run: build
	./$(BINARY) -addr $(ADDR)

test:
	$(GO) test $(GOFLAGS) ./...

race:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

# check reports formatting problems instead of fixing them, so it is safe to
# run where the working tree should not be touched.
check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

tidy:
	$(GO) mod tidy

# CGO is off so the results run anywhere, including a scratch container.
dist: $(SOURCES)
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=$(DIST)/$(BINARY)-$$os-$$arch; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $$out . || exit 1; \
	done

clean:
	rm -f $(BINARY)
	rm -rf $(DIST)

help:
	@echo "build   build ./$(BINARY) (default; skipped when nothing changed)"
	@echo "run     build and start it, ADDR=$(ADDR)"
	@echo "test    go test ./..."
	@echo "race    go test -race -count=1 ./..."
	@echo "vet     go vet ./..."
	@echo "fmt     gofmt -w ."
	@echo "check   gofmt check, vet and race tests: the CI gate"
	@echo "tidy    go mod tidy"
	@echo "dist    static binaries in $(DIST)/ for: $(PLATFORMS)"
	@echo "clean   remove ./$(BINARY) and $(DIST)/"
