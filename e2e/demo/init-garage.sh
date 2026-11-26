#!/bin/sh
set -eu

garage() {
  /garage "$@"
}

mkdir -p /run
rm -f /run/demo-garage-ready
/garage server &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true' EXIT
trap 'exit 0' TERM INT

remaining=30
until status="$(garage status 2>/dev/null)"; do
  remaining=$((remaining - 1))
  if [ "$remaining" -le 0 ]; then
    echo "Garage RPC did not start" >&2
    exit 1
  fi
  sleep 1
done

if ! garage layout show | grep -Eq 'Current cluster layout version: [1-9]'; then
  node="$(printf '%s\n' "$status" | awk 'length($1) == 16 && $1 ~ /^[a-f0-9]+$/ {print $1; exit}')"
  garage layout assign -z example -c 2G "$node"
  garage layout apply --version 1
fi

access_key=GKad0d778308314ea0e87d5af7
gateway_key=GKb5c507b83b8b5bd04cf89337
if ! garage key info "$access_key" >/dev/null 2>&1; then
  garage key import --yes "$access_key" 8df1c8a56d450b93faef06ee6930aa05f6fbe14d862762ae4cbcee67acca89b0c
fi
if ! garage key info "$gateway_key" >/dev/null 2>&1; then
  garage key import --yes "$gateway_key" 31f2e4d39e715bf83c1328f1856e48f78df9d1158e7e409ee8f02a9326769bbd0
fi
for bucket in assets videos; do
  if ! garage bucket info "$bucket" >/dev/null 2>&1; then
    garage bucket create "$bucket"
  fi
  garage bucket allow --read --write "$bucket" --key "$access_key"
  garage bucket allow --read "$bucket" --key "$gateway_key"
done

touch /run/demo-garage-ready
wait "$server_pid"
