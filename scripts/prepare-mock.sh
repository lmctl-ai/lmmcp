#!/usr/bin/env bash
# Prepare only reviewed public lmmock fixtures under an explicit private prefix.
# usage: scripts/prepare-mock.sh [TAG] ABSOLUTE_DIR; no auth or global installs.
set -euo pipefail
if [ $# -eq 1 ]; then tag=v0.1.111; dest=$1; elif [ $# -eq 2 ]; then tag=$1; dest=$2; else echo 'usage: prepare-mock.sh [TAG] ABSOLUTE_DIR' >&2; exit 2; fi
case "$dest" in /*) ;; *) echo 'DIR must be absolute' >&2; exit 2;; esac
case "$tag" in
  v0.1.111) source_commit=91d832a8e4d8d0f5c7f9b709fb372ad45b381b42; sums_digest=2cea99c81c49f4c16c38987c27f7fa370b28a711e82067eb9a18564905e49f5c;;
  v0.1.110) source_commit=025eaed53b5ea7c6b568681fec8dc6deb821ed21; sums_digest=ce735f274a0ece8cce847500816487573488203f2fffc5a6d6e856e8e18c623f;;
  *) echo 'unreviewed mock release; use v0.1.110 or v0.1.111' >&2; exit 2;;
esac
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) asset=lmmock-linux-x64;; Linux-aarch64) asset=lmmock-linux-arm64;;
  Darwin-arm64) asset=lmmock-macos-arm64;; Darwin-x86_64) asset=lmmock-macos-x64;;
  *) echo 'unsupported platform' >&2; exit 2;;
esac
mkdir -p "$dest/dl" "$dest/build"
python3 - "$tag" "$asset" "$sums_digest" "$dest" <<'PY'
import hashlib,json,pathlib,sys,urllib.request
version,asset,pin,directory=sys.argv[1:];root=pathlib.Path(directory);base='https://github.com/lmctl-ai/lmmock/releases/download/'+version+'/'
def download(name):
 return urllib.request.urlopen(urllib.request.Request(base+name,headers={'User-Agent':'lmmcp-public-ci'})).read()
manifest=download('SHA256SUMS')
if hashlib.sha256(manifest).hexdigest()!=pin:raise SystemExit('mock checksum manifest does not match its reviewed pin')
sums={parts[1].lstrip('*'):parts[0] for line in manifest.decode().splitlines() if len(parts:=line.split())==2}
if asset not in sums:raise SystemExit('mock asset is absent from the pinned checksum manifest')
binary=download(asset)
if hashlib.sha256(binary).hexdigest()!=sums[asset]:raise SystemExit('mock binary checksum mismatch')
(root/'dl/SHA256SUMS').write_bytes(manifest);(root/'dl'/asset).write_bytes(binary);(root/'lmmock').write_bytes(binary);(root/'lmmock').chmod(0o755)
(root/'download-verification.json').write_text(json.dumps({'release':version,'asset':asset,'sha256':sums[asset],'manifestSHA256':pin},indent=2)+'\n')
PY
if [ ! -d "$dest/src/.git" ]; then git clone -q --depth 1 --branch "$tag" https://github.com/lmctl-ai/lmmock.git "$dest/src"; fi
test "$(git -C "$dest/src" rev-parse HEAD)" = "$source_commit"
git -C "$dest/src" diff --quiet HEAD --
# The mock compiler needs no account configuration. Keep npm away from user files.
for name in npm-user.conf npm-global.conf; do
  if [ -s "$dest/$name" ]; then echo 'mock npm config must be empty' >&2; exit 2; fi
  touch "$dest/$name"
done
export NPM_CONFIG_USERCONFIG="$dest/npm-user.conf" NPM_CONFIG_GLOBALCONFIG="$dest/npm-global.conf" NPM_CONFIG_CACHE="$dest/npm-cache"
if [ ! -x "$dest/compiler/node_modules/.bin/lmscript" ]; then npm install --prefix "$dest/compiler" @lmctl-ai/lmctl@0.2.468 --no-audit --no-fund >&2; fi
LMSCRIPT_BIN="$dest/compiler/node_modules/.bin/lmscript" node "$dest/src/scripts/capture-fixture.mjs" "$dest/src/examples/mcp/server.lms" "$dest/build/mcp.lmcode" >&2
echo "export LMMCP_TEST_LMMOCK=$dest/lmmock LMMCP_TEST_PROGRAM=$dest/build/mcp.lmcode"
