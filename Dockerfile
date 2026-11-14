# 多阶段构建：仅产出 Linux 单文件二进制，镜像很小
# Multi-stage build: produces a small Linux-only image.
FROM golang:1.21-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/mcnode .

FROM alpine:3.19
RUN adduser -D -H mcnode
COPY --from=build /out/mcnode /usr/local/bin/mcnode
USER mcnode
EXPOSE 7001
# 挂载配置到 /etc/mcnode/config.jsonc（含 multipath.agent_token 与 agent_mc）
ENTRYPOINT ["mcnode", "--agent", "--config", "/etc/mcnode/config.jsonc"]
