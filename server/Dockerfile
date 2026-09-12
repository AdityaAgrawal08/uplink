FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /uplink-server .

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
COPY --from=builder /uplink-server /usr/local/bin/uplink-server

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["uplink-server"]
