#!/bin/bash
# Apply all mflux 0.15.5 dreambooth training patches, in order.
#
# Usage: ./apply.sh <venv-site-packages>
#   e.g. ./apply.sh ~/flux-train-venv/lib/python3.11/site-packages
#
# The patches are order-dependent (04 anchors on 03's output and reverts a
# 02 hook) and each script asserts its anchor text, so a fresh 0.15.5 venv
# applies cleanly and any already-patched venv fails fast instead of
# double-applying. If an assert fires mid-way, the venv is inconsistent:
# recreate it (see README) rather than continuing.
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
SP="${1:?usage: apply.sh <venv-site-packages>}"
if [ ! -d "$SP/mflux" ]; then
  echo "not a mflux site-packages dir: $SP" >&2
  exit 1
fi

for patch in "$DIR"/0*.py; do
  echo "== $(basename "$patch")"
  python3 "$patch" "$SP"
done

# Stale bytecode would keep running the pre-patch code.
find "$SP/mflux" -name '__pycache__' -type d -exec rm -rf {} + 2>/dev/null || true

echo "applied all patches; smoke-test with:"
echo "  \"$SP/../../../bin/python\" -c 'from mflux.models.flux.variants.dreambooth.dreambooth import DreamBooth; print(\"ok\")'"