#!/usr/bin/env bash
# Download the digest-verified lmmock release asset and compile its MCP example into a private directory.
#   usage: scripts/prepare-mock.sh [TAG] ABSOLUTE_PRIVATE_DIR      (TAG defaults to v0.1.111; prints `export` lines)
# Needs: gh, git, node, npm. Nothing is installed globally; the compiler is installed under DIR.
set -euo pipefail
if [ $# -eq 1 ]; then tag=v0.1.111; dest=$1; else tag=$1; dest=$2; fi
case $dest in /*) ;; *) echo "DIR must be absolute" >&2; exit 2;; esac
LMCTL=0.2.468
mkdir -p "$dest"
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) asset=lmmock-linux-x64;; Linux-aarch64) asset=lmmock-linux-arm64;;
  Darwin-arm64) asset=lmmock-macos-arm64;; Darwin-x86_64) asset=lmmock-macos-x64;;
  *) echo "unsupported platform" >&2; exit 2;;
esac
gh release download "$tag" -R lmctl-ai/lmmock -p "$asset" -p SHA256SUMS -D "$dest/dl" --clobber
( cd "$dest/dl" && sha256sum -c --ignore-missing SHA256SUMS >&2 )
if [ "$tag" = v0.1.111 ] && [ "$asset" = lmmock-linux-x64 ]; then
  echo "fe68bedc826ba36f89206923c4e1c7a055fb14b36fca98ac5fb02e6e918d61b3  $dest/dl/$asset" | sha256sum -c - >&2
fi
install -m 0755 "$dest/dl/$asset" "$dest/lmmock"
rm -rf "$dest/src"
git clone -q --branch "$tag" https://github.com/lmctl-ai/lmmock.git "$dest/src"
[ -x "$dest/compiler/node_modules/.bin/lmscript" ] || npm install --prefix "$dest/compiler" "@lmctl-ai/lmctl@$LMCTL" --no-audit --no-fund >&2
mkdir -p "$dest/build"
LMSCRIPT_BIN="$dest/compiler/node_modules/.bin/lmscript" node "$dest/src/scripts/capture-fixture.mjs" "$dest/src/examples/mcp/server.lms" "$dest/build/mcp.lmcode" >&2
echo "export LMMCP_TEST_LMMOCK=$dest/lmmock LMMCP_TEST_PROGRAM=$dest/build/mcp.lmcode"
