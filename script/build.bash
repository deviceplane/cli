#!/bin/bash
set -e

if [ -z "$CLI_VERSION" ]; then
    echo "CLI_VERSION not set"
    exit 1
fi

mkdir -p ./dist/cli

OS_PLATFORM_ARG=(linux windows darwin)

declare -a OS_ARCH_ARG
OS_ARCH_ARG[linux]="amd64 arm arm64"
OS_ARCH_ARG[windows]="386 amd64"
OS_ARCH_ARG[darwin]="amd64"

for OS in ${OS_PLATFORM_ARG[@]}; do
    for ARCH in ${OS_ARCH_ARG[${OS}]}; do
        OUTPUT_BIN="dist/cli/$CLI_VERSION/$OS/$ARCH/deviceplane"
        if test "$OS" = "windows"; then
            OUTPUT_BIN="${OUTPUT_BIN}.exe"
        fi
        echo "Building binary for $OS/$ARCH..."
        GOOS=$OS GOARCH=$ARCH CGO_ENABLED=0 go build \
              -mod vendor \
              -ldflags="-s -w -X main.version=$CLI_VERSION" \
              -o ${OUTPUT_BIN} ./cmd/deviceplane
    done
done

