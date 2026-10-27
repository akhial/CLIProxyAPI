#!/usr/bin/env bash
# Atomic production upgrade: swap the backend image (and optionally the
# management UI), health-check, and roll back automatically on any failure.
#
# Usage: fork/upgrade.sh --image <tag> [--ui <management.html>] [--check]
#   --check   preflight + staging only; changes nothing.
# Overridable for rehearsals: PROD, CONTAINER, BASE, PUBLIC. See fork/PLAYBOOK.md.
set -Eeuo pipefail

PROD=${PROD:-/srv/work/cliproxyapi}
CONTAINER=${CONTAINER:-cli-proxy-api}
BASE=${BASE:-http://127.0.0.1:8317}
PUBLIC=${PUBLIC:-https://baghdad.komodo-spectrum.ts.net:8317}
NEW_IMAGE=''
UI_FILE=''
CHECK_ONLY=0
while (($#)); do
  case $1 in
    --image) NEW_IMAGE=$2; shift 2 ;;
    --ui) UI_FILE=$(realpath "$2"); shift 2 ;;
    --check) CHECK_ONLY=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done
[[ -n $NEW_IMAGE ]] || { echo "usage: upgrade.sh --image <tag> [--ui <file>] [--check]" >&2; exit 64; }

TS=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP=$PROD/backups/pre-upgrade-$TS
LOG=$(mktemp)
log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }
die() {
  log "ABORT: $*"
  exit 1
}

cd "$PROD"
STAGED=(compose.yaml.upgrade-new static/management.html.upgrade-new)
trap 'rm -f "${STAGED[@]}"' EXIT

# ---------------------------------------------------------------- preflight
log "preflight"
for f in compose.yaml config/config.yaml static/management.html credentials.txt; do
  [[ -f $f ]] || die "missing $PROD/$f"
done
IMAGE_RE='^([[:space:]]+)image:[[:space:]]*(.+)$'
[[ $(grep -Ec "$IMAGE_RE" compose.yaml) == 1 ]] || die "compose.yaml must have exactly one image line"
CURRENT_IMAGE=$(sed -nE "s/$IMAGE_RE/\\2/p" compose.yaml)
[[ $CURRENT_IMAGE != "$NEW_IMAGE" ]] || die "$NEW_IMAGE is already configured"
NEW_IMAGE_ID=$(docker image inspect -f '{{.Id}}' "$NEW_IMAGE") || die "image $NEW_IMAGE not found"
if [[ -n $UI_FILE ]]; then
  [[ -s $UI_FILE ]] || die "UI file $UI_FILE missing or empty"
  UI_SHA=$(sha256sum "$UI_FILE" | cut -d' ' -f1)
fi

MGMT_KEY=$(sed -n 's/^Management key: //p' credentials.txt)
API_KEY=$(sed -n 's/^API key: //p' credentials.txt)
[[ -n $MGMT_KEY && -n $API_KEY ]] || die "could not read keys from credentials.txt"
api_ok() { curl -fsS -o /dev/null --max-time 10 -H "Authorization: Bearer $API_KEY" "$BASE/v1/models"; }
mgmt() { curl -fsS --max-time 10 -H "Authorization: Bearer $MGMT_KEY" "$BASE/v8/management/$1"; }
api_ok || die "current deployment is not healthy; refusing to upgrade on top of it"
STRATEGY=$(mgmt config | python3 -c 'import json,sys; print(json.load(sys.stdin).get("routing",{}).get("strategy",""))') ||
  die "could not read the routing strategy"
log "current: $CURRENT_IMAGE (strategy ${STRATEGY:-default}); target: $NEW_IMAGE"

# ---------------------------------------------------------------- stage
cp -p compose.yaml compose.yaml.upgrade-new
sed -E -i "s#$IMAGE_RE#\\1image: $NEW_IMAGE#" compose.yaml.upgrade-new
[[ $(diff compose.yaml compose.yaml.upgrade-new | grep -c '^[<>]') == 2 ]] || die "compose diff is not a single image change"
docker compose -f compose.yaml.upgrade-new config -q || die "staged compose.yaml is invalid"
if [[ -n $UI_FILE ]]; then
  install -m 0644 "$UI_FILE" static/management.html.upgrade-new
  [[ $(sha256sum static/management.html.upgrade-new | cut -d' ' -f1) == "$UI_SHA" ]] || die "staged UI corrupt"
fi
log "staged files validated"
if [[ $CHECK_ONLY == 1 ]]; then
  log "check only: no changes made"
  exit 0
fi

mkdir -m 700 -p "$BACKUP"
cp -p compose.yaml config/config.yaml static/management.html "$BACKUP/"
log "backup in $BACKUP"

# ---------------------------------------------------------------- swap + verify
healthy() {
  local i page logs
  for i in $(seq 1 30); do
    [[ $(docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null) == true ]] && api_ok && break
    sleep 2
  done
  api_ok || { log "API not answering"; return 1; }
  [[ $(docker inspect -f '{{.Image}}' "$CONTAINER") == "$NEW_IMAGE_ID" ]] || { log "wrong image running"; return 1; }
  page=$(curl -fsS --max-time 10 "$BASE/management.html" | sha256sum | cut -d' ' -f1) || { log "UI not served"; return 1; }
  if [[ -n $UI_FILE && $page != "$UI_SHA" ]]; then
    log "served UI differs from $UI_FILE"
    return 1
  fi
  mgmt quota/history 2>/dev/null | python3 -c 'import json,sys; assert json.load(sys.stdin)["enabled"] is True' 2>/dev/null ||
    { log "quota history endpoint not working"; return 1; }
  mgmt config 2>/dev/null | python3 -c "import json,sys; assert json.load(sys.stdin).get('routing',{}).get('strategy','') == '$STRATEGY'" 2>/dev/null ||
    { log "routing strategy changed"; return 1; }
  logs=$(docker logs "$CONTAINER" 2>&1)
  [[ $logs == *'quota history: recording'* ]] || { log "collector did not start"; return 1; }
}

rollback() {
  log "ROLLING BACK to $CURRENT_IMAGE"
  set +e
  trap - ERR
  docker compose stop
  cp -p "$BACKUP/compose.yaml" compose.yaml
  cp -p "$BACKUP/config.yaml" config/config.yaml
  cp -p "$BACKUP/management.html" static/management.html
  docker compose up -d --pull never
  local i
  for i in $(seq 1 30); do
    api_ok && break
    sleep 2
  done
  if api_ok; then log "rolled back; previous deployment is healthy"; else log "ROLLBACK STARTED BUT API NOT ANSWERING"; fi
  cp "$LOG" "$BACKUP/upgrade.log"
  exit 1
}

log "stopping $CONTAINER"
docker compose stop || die "could not stop the container (nothing changed)"
trap rollback ERR
mv -f compose.yaml.upgrade-new compose.yaml
[[ -z $UI_FILE ]] || mv -f static/management.html.upgrade-new static/management.html
log "starting $NEW_IMAGE"
docker compose up -d --pull never
healthy || rollback
trap - ERR

if curl -fsS -o /dev/null --max-time 15 -H "Authorization: Bearer $API_KEY" "$PUBLIC/v1/models"; then
  log "tailnet HTTPS OK"
else
  log "WARNING: tailnet HTTPS check failed (local checks passed)"
fi
log "upgraded to $NEW_IMAGE; backup in $BACKUP"
cp "$LOG" "$BACKUP/upgrade.log"
