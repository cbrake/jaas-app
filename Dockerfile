FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.version=${VERSION}" -o /out/jaas-app .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
	&& adduser -D -u 10001 jaas \
	&& mkdir -p /data && chown jaas:jaas /data
COPY --from=build /out/jaas-app /usr/local/bin/jaas-app
USER jaas
ENV LISTEN_ADDR=:8370 \
	DB_PATH=/data/jaas.db
VOLUME ["/data"]
EXPOSE 8370
ENTRYPOINT ["/usr/local/bin/jaas-app"]
CMD ["serve"]
