.PHONY: test run docker

# what every commit must pass
test:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }
	go vet ./...
	go test -race ./...

run:
	go run ./cmd/intelligence

docker:
	docker build -t intelligence .
