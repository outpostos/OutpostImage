.PHONY: build test lint clean docker-build docker-push test-coverage ci

IMAGE_NAME ?= ghcr.io/outpostos/outpostimage
IMAGE_TAG  ?= latest

build:
	go build -buildvcs=false -o bin/outpostimage .

test:
	go test ./...

lint:
	@which golangci-lint > /dev/null 2>&1 && golangci-lint run --timeout=5m || echo "golangci-lint not installed, skipping"

clean:
	rm -rf bin/ coverage.out coverage.html

docker-build:
	docker build -t $(IMAGE_NAME):$(IMAGE_TAG) .

docker-push:
	docker push $(IMAGE_NAME):$(IMAGE_TAG)

test-coverage:
	go test -coverprofile=coverage.out -covermode=atomic ./pkg/... ./internal/...
	go tool cover -func=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

ci: lint test-coverage
	@echo "CI complete"
