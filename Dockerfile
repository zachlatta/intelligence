FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/intelligence ./cmd/intelligence

# alpine, not distroless: Coolify's health check runs through /bin/sh and wget in the container
FROM alpine:3
RUN adduser -D -H -u 65532 app
COPY --from=build /out/intelligence /usr/local/bin/intelligence
USER app
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/intelligence"]
