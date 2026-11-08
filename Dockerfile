FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Coolify passes the deployed commit (include_source_commit_in_build); every binary carries it
ARG SOURCE_COMMIT=dev
RUN set -eu; ldflags="-s -w -X main.buildCommit=${SOURCE_COMMIT}"; \
    CGO_ENABLED=0 go build -trimpath -ldflags="$ldflags" -o /out/intelligence ./cmd/intelligence; \
    mkdir -p /out/host; \
    for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
      CGO_ENABLED=0 GOOS=${p%/*} GOARCH=${p#*/} go build -trimpath -ldflags="$ldflags" \
        -o /out/host/intelligence-${p%/*}-${p#*/} ./cmd/intelligence; \
    done

# Every test passes, the read-only SQL contract with them, against a throwaway Postgres 18,
# before an image exists: a failure fails the build, so Coolify never deploys it.
FROM postgres:18-alpine AS test
COPY --from=build /usr/local/go /usr/local/go
COPY --from=build /go/pkg/mod /go/pkg/mod
ENV PATH=/usr/local/go/bin:$PATH GOPATH=/go GOTOOLCHAIN=local GOFLAGS=-mod=mod CGO_ENABLED=0
COPY --from=build /src /src
WORKDIR /src
RUN set -eu; \
    mkdir -p /run/pgtest && chown postgres /run/pgtest; \
    su postgres -c "initdb -D /run/pgtest/data -U postgres --auth=trust >/dev/null"; \
    su postgres -c "pg_ctl -D /run/pgtest/data -o '-k /run/pgtest -c listen_addresses=127.0.0.1' -w start >/dev/null"; \
    TEST_DATABASE_URL="postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable" go test -count=1 ./...; \
    su postgres -c "pg_ctl -D /run/pgtest/data -m fast stop >/dev/null"; \
    touch /tests-passed

# alpine, not distroless: Coolify's health check runs through /bin/sh and wget in the container
FROM alpine:3
RUN adduser -D -H -u 65532 app
COPY --from=test /tests-passed /usr/local/share/intelligence/tests-passed
COPY --from=build /out/host /usr/local/share/intelligence/host
COPY --from=build /out/intelligence /usr/local/bin/intelligence
USER app
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/intelligence"]
