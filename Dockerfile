FROM golang:1.23-alpine AS builder

ARG TARGETARCH

ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /app
COPY go.mod .
COPY collector.go .
RUN go mod tidy

RUN echo "Building for arch: $TARGETARCH"
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o edge-collector collector.go

FROM alpine:3.18

RUN apk add --no-cache bash ca-certificates i2c-tools bc

WORKDIR /app
COPY --from=builder /app/edge-collector /app/edge-collector
RUN chmod +x /app/edge-collector

CMD ["/app/edge-collector"]