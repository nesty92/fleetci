#!/bin/sh
# fleetci's installer, served by a controller: fleetci itself, then
# fleetci join -service. fleetci token create prints the line that runs it:
#
#   curl -fsSk --pinnedpubkey <the controller's key> https://<controller>:9481/v1/install.sh |
#     sh -s -- <controller> <token> [fleetci join flags]
#
# curl checks the controller by the key it pins, not by a CA (-k): the
# fleet's CA is in no system's store. This script checks fleetci by its
# sum, and the join checks the rest.
set -eu
ARCH='{{arch}}'
SUM='{{sum}}'
PIN='{{pin}}'
PORT='{{port}}'
BIN=/usr/local/bin/fleetci

die() { echo "fleetci install: $*" >&2; exit 1; }
[ $# -ge 2 ] || die "usage: sh -s -- <controller> <token> [fleetci join flags]"
CTL=$1
FLEETCI_JOIN_TOKEN=$2
shift 2
export FLEETCI_JOIN_TOKEN
case $CTL in
*:*) URL=https://$CTL ;;
*) URL=https://$CTL:$PORT ;;
esac

[ "$(id -u)" = 0 ] || die "run it as root"
case $(uname -m) in
x86_64 | amd64) a=amd64 ;;
aarch64 | arm64) a=arm64 ;;
*) a=$(uname -m) ;;
esac
[ "$a" = "$ARCH" ] || die "this machine is $a; this controller has fleetci for $ARCH only"
[ -d /run/systemd/system ] || die "no systemd here: install fleetci yourself, then fleetci join, then run fleetci worker"

# What fleetci join needs and doesn't install: nftables and iproute2.
apt= dnf=
command -v nft >/dev/null 2>&1 || { apt="$apt nftables"; dnf="$dnf nftables"; }
command -v ip >/dev/null 2>&1 || { apt="$apt iproute2"; dnf="$dnf iproute"; }
if [ -n "$apt" ]; then
  if command -v apt-get >/dev/null 2>&1; then
    echo "ok    packages      $apt"
    DEBIAN_FRONTEND=noninteractive apt-get -qq update
    DEBIAN_FRONTEND=noninteractive apt-get -qq install -y $apt >/dev/null
  elif command -v dnf >/dev/null 2>&1; then
    echo "ok    packages      $dnf"
    dnf -q -y install $dnf
  else
    die "install$apt first"
  fi
fi

sum() { sha256sum "$1" 2>/dev/null | cut -d' ' -f1; }
if [ "$(sum $BIN || true)" = "$SUM" ]; then
  echo "ok    install        fleetci is current"
else
  mkdir -p "$(dirname $BIN)"
  tmp=$(mktemp "$BIN.XXXXXX")
  trap 'rm -f "$tmp"' EXIT
  curl -fsSk --pinnedpubkey "$PIN" -H "Authorization: Bearer $FLEETCI_JOIN_TOKEN" \
    -o "$tmp" "$URL/v1/install/fleetci" || die "could not download fleetci from $URL"
  [ "$(sum "$tmp")" = "$SUM" ] || die "the fleetci downloaded is not the controller's: its sum differs"
  chmod 755 "$tmp"
  mv "$tmp" $BIN
  trap - EXIT
  echo "ok    install        fleetci into $(dirname $BIN)"
fi
exec $BIN join -service "$@" "$CTL"
