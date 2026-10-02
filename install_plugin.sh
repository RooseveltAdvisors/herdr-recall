#!/usr/bin/env bash
# Build herdr-recall from source and link it into the local Herdr install.
# This is the only build path the plugin ships: it keeps the go build flags,
# the output name, and the link step in one place.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

mkdir -p bin
go build -o bin/herdr-recall .

if herdr plugin list 2>/dev/null | grep -q "RooseveltAdvisors.herdr-recall"; then
  echo "already linked: RooseveltAdvisors.herdr-recall"
else
  herdr plugin link "$DIR"
fi

echo "built and linked RooseveltAdvisors.herdr-recall"
echo "next: add the prefix+shift+l binding to config.toml (see README), then"
echo "      herdr server reload-config"
