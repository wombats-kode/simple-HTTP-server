.PHONY: build run run-secure run-insecure gen-cert fmt test vet clean-binaries clean

BINARY=serve
BUILD_DIR=bin

build:
	mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY) .

run: build
	./bin/serve

run-secure: build
	./bin/serve

run-insecure: build
	./bin/serve -insecure

gen-cert:
	go run . -gencert -cert=certs/server.pem -key=certs/server.key

fmt:
	gofmt -w .

test:
	go test ./...

vet:
	go vet ./...

clean-binaries:
	rm -f $(BINARY)
	rm -rf $(BUILD_DIR)

clean: clean-binaries
	rm -rf certs
