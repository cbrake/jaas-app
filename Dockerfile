# Toolchain and runtime are pinned by digest; bump them deliberately.
FROM golang:1.26-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .

# `docker build --target test .` runs the suite; the release stage below does
# not depend on it, so deploy.sh runs it explicitly first.
FROM build AS test
RUN test -z "$(gofmt -s -l .)" && go vet ./... && go test -count=1 ./...

FROM build AS release-build
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/jaas-app .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=release-build /out/jaas-app /app/jaas-app
ENV LISTEN_ADDR=:8370 \
	DB_PATH=/data/jaas.db
EXPOSE 8370
USER 65532:65532
ENTRYPOINT ["/app/jaas-app"]
CMD ["serve"]
