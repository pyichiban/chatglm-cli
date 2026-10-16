#!/usr/bin/env bash
# 构建 chatglm-cli（命令名 glm2cli）：win/linux 双平台单文件，CGO 关闭、零第三方依赖。
# 版本号在 internal/cli/cli.go 的 VERSION 常量里（const 不能用 -X 注入，改版本请直接改代码）。
set -euo pipefail
cd "$(dirname "$0")/.."

go vet ./... && go test ./...

BIN=glm2cli
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "dist/${BIN}.exe" ./cmd/${BIN}
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "dist/${BIN}"    ./cmd/${BIN}

ls -la dist
echo "本地安装到 PATH：go install ./cmd/${BIN}   （产出 ${BIN}，需仓库目录在 GOPATH 语义下可用）"
