# Builds the OPC XML-DA test server used by test/dockerintegration's
# Docker-based integration test. Build context must be the repository
# root (so both the main module and this test module's go.mod are
# visible), e.g.:
#
#   docker build -f test/dockerintegration/Dockerfile -t gopcxmlda-server-dockerintegration .
#
# testcontainers-go (see docker_test.go) does this automatically.

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY . .
WORKDIR /src/test/dockerintegration
RUN CGO_ENABLED=0 go build -o /out/server ./server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
