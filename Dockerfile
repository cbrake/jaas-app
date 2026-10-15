# Toolchain and runtime are pinned by digest; bump them deliberately.
FROM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .

# `docker build --target test .` runs the suite; the release stage below does
# not depend on it, so run it before building a release.
FROM build AS test
RUN test -z "$(gofmt -s -l .)" && go vet ./... && go test -count=1 ./...

FROM build AS release-build
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/jaas-app . && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=release-build /out/jaas-app /app/jaas-app
# /data is owned by the runtime user so a fresh named volume is writable.
COPY --from=release-build --chown=65532:65532 /out/data /data
ENV LISTEN_ADDR=:8370 \
	DB_PATH=/data/jaas.db
VOLUME ["/data"]
EXPOSE 8370
USER 65532:65532
ENTRYPOINT ["/app/jaas-app"]
CMD ["serve"]
