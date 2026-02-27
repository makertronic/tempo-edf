#!/bin/bash
set -e

echo "=== Build Tempo EDF ==="
echo

echo "[1/2] macOS Intel (amd64)..."
CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -o TempoEDF_macos_intel .
echo "OK : TempoEDF_macos_intel"

echo "[2/2] macOS ARM64 (Apple Silicon)..."
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -o TempoEDF_macos_arm64 .
echo "OK : TempoEDF_macos_arm64"

echo
echo "=== Termine ==="
