.PHONY: test test-suite test-pkg run docker

# What every commit must pass. Database tests need TEST_DATABASE_URL (a superuser URL); without it,
# a throwaway Postgres runs in Docker for the length of the tests.
test:
	scripts/with-test-capacity $(MAKE) test-suite

# Internal target: the supervising test target holds capacity across formatting, vet and Postgres.
# go vet runs alongside the tests, not before them; the target fails if either does.
test-suite:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }
	@echo "go vet ./... & go test -race -timeout=30m ./..."; \
	go vet ./... & vet=$$!; \
	if [ -n "$$TEST_DATABASE_URL" ]; then go test -race -timeout=30m ./...; else scripts/with-test-postgres go test -race -timeout=30m ./...; fi; \
	tests=$$?; wait $$vet; vetted=$$?; \
	[ $$vetted -eq 0 ] || echo "go vet failed"; \
	[ $$tests -eq 0 ] && [ $$vetted -eq 0 ]

# Run one package's tests against their own throwaway Postgres, with the same capacity lease.
test-pkg:
	@test -n "$(PKG)" || { echo "usage: make test-pkg PKG=./internal/whatsnew"; exit 1; }
	scripts/with-test-postgres go test -race -timeout=30m "$(PKG)"

run:
	go run ./cmd/intelligence

docker:
	docker build -t intelligence .
