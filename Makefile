.PHONY: test test-suite run docker

# What every commit must pass. Database tests need TEST_DATABASE_URL (a superuser URL); without it,
# a throwaway Postgres runs in Docker for the length of the tests.
test:
	scripts/with-test-capacity $(MAKE) test-suite

# Internal target: the supervising test target holds capacity across formatting, vet and Postgres.
test-suite:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }
	go vet ./...
	@if [ -n "$$TEST_DATABASE_URL" ]; then go test -race -timeout=30m ./...; else scripts/with-test-postgres go test -race -timeout=30m ./...; fi

run:
	go run ./cmd/intelligence

docker:
	docker build -t intelligence .
