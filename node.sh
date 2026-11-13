#!/bin/sh
# Run a command in the Node toolchain container (Node is not installed on the host).
# Usage: ./node.sh npm test
# Cloud credentials and the shared SSH key are mounted at /aws and /nepho.
mkdir -p "$HOME/.aws" "$HOME/.config/nepho"
cd "$(dirname "$0")/typescript" && exec docker run --rm -i --network host \
  --cpuset-cpus="${NEPHO_CPUS:-14-27}" --cpuset-mems="${NEPHO_MEMS:-1}" \
  -u "$(id -u):$(stat -c %g /var/run/docker.sock)" -e HOME=/tmp/home \
  -v "$HOME/.aws":/aws:ro -e AWS_SHARED_CREDENTIALS_FILE=/aws/credentials -e AWS_CONFIG_FILE=/aws/config \
  -v "$HOME/.config/nepho":/nepho -e NEPHO_KEY=/nepho/id_rsa \
  $([ -f "$HOME/.config/nepho/gcp.json" ] && echo "-e GOOGLE_APPLICATION_CREDENTIALS=/nepho/gcp.json") \
  -v /var/run/docker.sock:/var/run/docker.sock -v "$PWD":/w -w /w node:22-bookworm "$@"
