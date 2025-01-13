FROM golang:1.22.2 AS builder

WORKDIR /app

COPY go.mod .
RUN go mod tidy

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dns-switcher .

FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/dns-switcher .

ENTRYPOINT ["./dns-switcher"]