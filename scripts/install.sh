#!/bin/bash
# Installs or upgrades macfleet-agent on a Mac, enrolls it, and starts it as a
# LaunchAgent. Run as the same non-root user that runs Nomad and Tart.
#
#   install.sh --server https://HOST:8443 --ca-sha256 HEX --token TOKEN [--version v0.1.0]
#   install.sh --upgrade                       # replace the binary, keep enrollment
#
# The release repository is public, so no token is needed. For a private fork,
# export GITHUB_TOKEN (read access to repository contents) first.
set -euo pipefail

REPO="${MACFLEET_REPO:-raghavharness/mac-fleet}"
VERSION="latest"
SERVER=""; PIN=""; TOKEN=""; UPGRADE=0
DIR="$HOME/.macfleet"
BIN="$DIR/bin/macfleet-agent"
LABEL="io.harness.macfleet-agent"

die() { echo "error: $*" >&2; exit 1; }
say() { echo "==> $*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="$2"; shift 2 ;;
    --ca-sha256) PIN="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --upgrade) UPGRADE=1; shift ;;
    -h|--help) sed -n 2,12p "$0"; exit 0 ;;
    *) die "unknown option $1" ;;
  esac
done

[ "$(uname -s)" = "Darwin" ] || die "this installer is for macOS"
[ "$(id -u)" != "0" ] || die "run as the non-root user that runs Nomad and Tart, not root"
case "$(uname -m)" in
  arm64) ARCH=arm64 ;;
  x86_64) ARCH=amd64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
if [ "$UPGRADE" = 0 ] && [ ! -f "$DIR/agent.json" ]; then
  [ -n "$SERVER" ] && [ -n "$PIN" ] && [ -n "$TOKEN" ] || die "first install needs --server, --ca-sha256 and --token (create them in the dashboard under Settings)"
fi

ASSET="macfleet-agent-darwin-$ARCH"
API="https://api.github.com/repos/$REPO"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

auth=()
[ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
curl_gh() { curl -fsSL ${auth[@]+"${auth[@]}"} "$@"; }

# Reads fields out of the release JSON with the JavaScript runtime built into macOS.
jxa() { osascript -l JavaScript -e 'function run(a){var r=JSON.parse($.NSString.stringWithContentsOfFileEncodingError(a[0],4,null).js);'"$1"'}' "$TMP/rel.json"; }

if [ -z "${GITHUB_TOKEN:-}" ]; then
  # Public repository: plain release download URLs, no API or JSON parsing.
  if [ "$VERSION" = "latest" ]; then BASE="https://github.com/$REPO/releases/latest/download"; else BASE="https://github.com/$REPO/releases/download/$VERSION"; fi
  TAG="$VERSION"
  say "Downloading $ASSET ($VERSION) from $REPO"
  curl -fsSL -o "$TMP/$ASSET" "$BASE/$ASSET" || die "download failed. Check the version exists at https://github.com/$REPO/releases (for a private fork, export GITHUB_TOKEN)."
  curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS" || die "could not download SHA256SUMS"
else
  say "Looking up release $VERSION of $REPO"
  if [ "$VERSION" = "latest" ]; then URL="$API/releases/latest"; else URL="$API/releases/tags/$VERSION"; fi
  curl_gh -H "Accept: application/vnd.github+json" -o "$TMP/rel.json" "$URL" || die "could not read the release from $REPO"
  TAG="$(jxa 'return r.tag_name;')"
  asset_id() { jxa "var x=r.assets.filter(function(s){return s.name==='$1'})[0]; return x?String(x.id):'';"; }
  BIN_ID="$(asset_id "$ASSET")"; SUM_ID="$(asset_id SHA256SUMS)"
  [ -n "$BIN_ID" ] && [ -n "$SUM_ID" ] || die "release $TAG has no $ASSET or SHA256SUMS"
  say "Downloading $ASSET $TAG"
  curl_gh -H "Accept: application/octet-stream" -o "$TMP/$ASSET" "$API/releases/assets/$BIN_ID"
  curl_gh -H "Accept: application/octet-stream" -o "$TMP/SHA256SUMS" "$API/releases/assets/$SUM_ID"
fi

say "Verifying checksum"
WANT="$(awk -v f="$ASSET" '$2==f || $2=="*"f {print $1}' "$TMP/SHA256SUMS")"
GOT="$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')"
[ -n "$WANT" ] && [ "$WANT" = "$GOT" ] || die "checksum mismatch for $ASSET"

say "Installing to $BIN"
mkdir -p "$DIR/bin"
chmod 755 "$TMP/$ASSET"
mv -f "$TMP/$ASSET" "$BIN"
xattr -d com.apple.quarantine "$BIN" 2>/dev/null || true
"$BIN" version

if [ "$UPGRADE" = 0 ] && [ -n "$TOKEN" ]; then
  say "Enrolling with $SERVER"
  "$BIN" enroll --server "$SERVER" --ca-sha256 "$PIN" --token "$TOKEN"
fi

say "Installing the LaunchAgent"
"$BIN" install-launchd >/dev/null
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
DOMAIN="gui/$(id -u)"
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
if ! launchctl bootstrap "$DOMAIN" "$PLIST" 2>/dev/null; then
  launchctl load -w "$PLIST"
fi
launchctl kickstart -k "$DOMAIN/$LABEL" 2>/dev/null || true

say "Done. The agent should appear in the dashboard within a few seconds."
echo "    logs:    tail -f $DIR/agent.log"
echo "    status:  launchctl print $DOMAIN/$LABEL | head"
echo "    upgrade: re-run this installer with --upgrade"
