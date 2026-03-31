# 第一阶段：编译
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY test.go .
# 使用交叉编译，确保根据目标架构生成二进制文件
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o edge-logger test.go

# 第二阶段：运行
FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/edge-logger .
# 设置时区为上海，确保日志时间与宿主机一致
RUN apk add --no-cache tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone
CMD ["./edge-logger"]