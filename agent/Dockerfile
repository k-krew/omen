FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /agent ./cmd/agent/main.go

FROM alpine:3.21
RUN apk add --no-cache iproute2
COPY --from=builder /agent /agent
ENTRYPOINT ["/agent"]
