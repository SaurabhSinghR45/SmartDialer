# Multi-stage production build for SmartDialer
FROM golang:1.22-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/dialer cmd/dialer/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /bin/simulator cmd/simulator/main.go

# Minimal distroless/scratch runtime
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /bin/dialer /usr/local/bin/dialer
COPY --from=builder /bin/simulator /usr/local/bin/simulator

EXPOSE 8080

ENTRYPOINT ["dialer"]
