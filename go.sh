#!/bin/sh
# Run a command in the Go toolchain container (Go is not installed on the host).
# Usage: ./go.sh go test ./...
mkdir -p "$HOME/.cache/nepho-go"
# Cloud credentials and the shared SSH key are mounted at /aws and /nepho.
mkdir -p "$HOME/.aws" "$HOME/.config/nepho"
cd "$(dirname "$0")/go" && exec docker run --rm -i --network host \
  --cpuset-cpus="${NEPHO_CPUS:-14-27}" --cpuset-mems="${NEPHO_MEMS:-1}" \
  -u "$(id -u):$(stat -c %g /var/run/docker.sock)" -e HOME=/cache -e GOPATH=/cache/gopath -e GOCACHE=/cache/build \
  -v "$HOME/.cache/nepho-go":/cache \
  -v "$HOME/.aws":/aws:ro -e AWS_SHARED_CREDENTIALS_FILE=/aws/credentials -e AWS_CONFIG_FILE=/aws/config \
  -v "$HOME/.config/nepho":/nepho -e NEPHO_KEY=/nepho/id_rsa \
  -v /var/run/docker.sock:/var/run/docker.sock -v "$PWD":/w -w /w golang:1.26-bookworm "$@"
