#!/usr/bin/env sh
# Render every Mermaid source in this folder to PNG (white background, 2x scale).
# Needs Node.js; mermaid-cli downloads a headless Chrome on first use.
set -eu
cd "$(dirname "$0")"
for src in *.mmd; do
  npx -y @mermaid-js/mermaid-cli@11 -c config.json -i "$src" -o "${src%.mmd}.png" -b white -s 2
done
