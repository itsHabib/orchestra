BINARY := orchestra
GOBIN  ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
  GOBIN = $(shell go env GOPATH)/bin
endif

.PHONY: build install uninstall test vet clean

build:
	go build -o $(BINARY) .

install: build
	cp $(BINARY) $(GOBIN)/$(BINARY)
	@echo "Installed $(BINARY) to $(GOBIN)/$(BINARY)"

uninstall:
	rm -f $(GOBIN)/$(BINARY)
	@echo "Removed $(BINARY) from $(GOBIN)"

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
