# Both binaries in one image: a cluster is bootstrap plus nodes, and giving them
# separate images would only mean building the same tree twice.
#
# The build context is the repository root (see docker-compose.yml), trimmed by
# the .dockerignore next to it.
FROM golang:1.22-alpine AS build

WORKDIR /src

# Dependencies first, so editing the sources does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY pkg ./pkg

# Static: the runtime stage has no libc to link against at load time, and a
# node that fails to start is a confusing way to learn that.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/rezoagwe-bootstrap ./cmd/bootstrap && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/rezoagwe-discovery ./cmd/discovery

FROM alpine:3.20

# busybox wget is what the compose healthchecks call; nothing else is needed.
COPY --from=build /out/rezoagwe-bootstrap /out/rezoagwe-discovery /usr/local/bin/

RUN adduser -D -u 10001 rezoagwe
USER rezoagwe

ENTRYPOINT ["rezoagwe-discovery"]
