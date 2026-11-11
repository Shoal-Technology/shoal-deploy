BINARY := shoal-deploy
LDFLAGS := -s -w

.PHONY: build test run clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

test:
	go test ./...

run: build
	./$(BINARY) --listen 127.0.0.1:7700 --data-dir ./data

clean:
	rm -f $(BINARY)
	rm -rf dist data
