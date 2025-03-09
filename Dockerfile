FROM golang:1.24 AS builder

ARG BUILD_VERSION

WORKDIR /build

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.Version=${BUILD_VERSION}" -o app .

FROM alpine:latest AS final

WORKDIR /app
COPY --from=builder /build/app /app/

RUN apk update && \
    apk add --no-cache sudo tzdata

# 设置所需的时区，例如亚洲/上海
ENV TZ=Asia/Shanghai

# 创建软链接，指向你想要的时区文件
RUN ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone

ENTRYPOINT ["/app/app"]