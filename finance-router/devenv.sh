#!/usr/bin/env bash
# 在只读的 $HOME 缓存下跑 Go：把构建/模块缓存都放工作区内。
# 用法：  source ./devenv.sh
#         go build ./... ; go run ./cmd/recon
#
# 为什么需要：本机 ~/.cache/go-build 与 ~/go/pkg 位于只读挂载，
# Go 默认写入会报 "read-only file system"。这里统一改到仓库内。
#
# -buildvcs=false：仓库未必是 git 仓，避免 VCS stamping 报错。
# CGO_ENABLED=0：本项目纯 Go（HTTP 客户端），关掉 cgo 可避免 ccache 写 /run 失败。

# shellcheck shell=bash
export GOCACHE="${GOCACHE:-$(pwd)/../.gocache}"
export GOMODCACHE="${GOMODCACHE:-$(pwd)/../.gomodcache}"
export GOSUMDB=off
export GOFLAGS="-mod=mod -buildvcs=false"
export CGO_ENABLED=0

echo "GOCACHE=$GOCACHE"
echo "GOMODCACHE=$GOMODCACHE"
echo "GOFLAGS=$GOFLAGS  CGO_ENABLED=$CGO_ENABLED"
