.PHONY: build test lint clean

build:
	go build -o bin/outpostimage .

test:
	go test ./...

lint:
	@which golangci-lint > /dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed, skipping"

clean:
	rm -rf bin/
