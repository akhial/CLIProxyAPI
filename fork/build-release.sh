#!/usr/bin/env bash
# Build a deployable release from both forks' main branches and smoke-test it.
#
# Usage: fork/build-release.sh [frontend-dir]
#   frontend-dir defaults to ../Cli-Proxy-API-Management-Center next to this repo.
# Output: local image cli-proxy-api-akhial:<release> and
#         $OUT_ROOT/<release>/{management.html,RELEASE.md}. See fork/PLAYBOOK.md.
set -euo pipefail

BACKEND=${BACKEND:-$(git -C "$(dirname "$0")" rev-parse --show-toplevel)}
FRONTEND=$(realpath "${1:-$BACKEND/../Cli-Proxy-API-Management-Center}")
OUT_ROOT=${OUT_ROOT:-/srv/work/cliproxyapi-release}

say() { printf '\n== %s\n' "$*"; }
die() {
  printf 'ABORT: %s\n' "$*" >&2
  exit 1
}

for repo in "$BACKEND" "$FRONTEND"; do
  git -C "$repo" fetch --quiet origin
  [[ $(git -C "$repo" branch --show-current) == main ]] || die "$repo is not on main"
  [[ -z $(git -C "$repo" status --porcelain) ]] || die "$repo has uncommitted changes"
  [[ $(git -C "$repo" rev-parse main) == $(git -C "$repo" rev-parse origin/main) ]] ||
    die "$repo main differs from origin/main"
done

# <upstream base tag>-akhial.<fork commit>: traceable to both upstream and the fork.
VERSION=$(git -C "$BACKEND" describe --tags --abbrev=0 main)-akhial.$(git -C "$BACKEND" rev-parse --short=8 main)
UI_VERSION=$(git -C "$FRONTEND" describe --tags --abbrev=0 main)-akhial.$(git -C "$FRONTEND" rev-parse --short=7 main)
# One release per backend+UI pair, so a UI-only change still gets its own image tag and folder.
# The server keeps reporting $VERSION.
RELEASE=$VERSION-ui.$(git -C "$FRONTEND" rev-parse --short=7 main)
IMAGE=cli-proxy-api-akhial:$RELEASE
OUT=$OUT_ROOT/$RELEASE
[[ ! -e $OUT ]] || die "$OUT already exists"

say "backend image $IMAGE"
docker build --quiet \
  --build-arg VERSION="$VERSION" \
  --build-arg COMMIT="$(git -C "$BACKEND" rev-parse --short main)" \
  --build-arg BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -t "$IMAGE" "$BACKEND" >/dev/null
IMAGE_ID=$(docker image inspect -f '{{.Id}}' "$IMAGE")

say "management UI $UI_VERSION"
(cd "$FRONTEND" && bun install --frozen-lockfile >/dev/null && VERSION=$UI_VERSION bun run build >/dev/null)
mkdir -p "$OUT"
cp "$FRONTEND/dist/index.html" "$OUT/management.html"
UI_SHA=$(sha256sum "$OUT/management.html" | cut -d' ' -f1)

say "smoke test"
SMOKE=$(mktemp -d)
NAME=cpa-release-smoke-$$
MGMT_KEY=smoke-$RANDOM$RANDOM
API_KEY=smoke-$RANDOM$RANDOM
cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  # The container writes as root; clean up from inside the image.
  docker run --rm -v "$SMOKE:/t" --entrypoint sh "$IMAGE" -c 'rm -rf /t/*' >/dev/null 2>&1 || true
  rmdir "$SMOKE" 2>/dev/null || true
}
trap cleanup EXIT
mkdir -p "$SMOKE/config" "$SMOKE/auths" "$SMOKE/static"
cp "$OUT/management.html" "$SMOKE/static/management.html"
cat >"$SMOKE/config/config.yaml" <<EOF
config-version: 8
server:
    port: 8317
management:
    allow-remote: true
    secret-key: "$MGMT_KEY"
    disable-auto-update-panel: true
access:
    api-keys:
        - "$API_KEY"
oauth:
    auth-dir: "/root/.cli-proxy-api"
routing:
    session-affinity: true
    strategy: "weekly-reset-weighted-round-robin"
EOF
docker run -d --name "$NAME" -p 127.0.0.1::8317 -e MANAGEMENT_STATIC_PATH=/CLIProxyAPI/static \
  -v "$SMOKE/config:/CLIProxyAPI/config" -v "$SMOKE/auths:/root/.cli-proxy-api" \
  -v "$SMOKE/static:/CLIProxyAPI/static" "$IMAGE" ./CLIProxyAPI --config /CLIProxyAPI/config/config.yaml >/dev/null
BASE=http://$(docker port "$NAME" 8317/tcp | head -1)
for _ in $(seq 1 30); do
  curl -fsS -o /dev/null -H "Authorization: Bearer $API_KEY" "$BASE/v1/models" 2>/dev/null && break
  sleep 2
done
curl -fsS -o /dev/null -H "Authorization: Bearer $API_KEY" "$BASE/v1/models" || die "API did not come up"
[[ $(curl -fsS "$BASE/management.html" | sha256sum | cut -d' ' -f1) == "$UI_SHA" ]] || die "UI not served"
curl -fsS -H "Authorization: Bearer $MGMT_KEY" "$BASE/v8/management/quota/history" |
  python3 -c 'import json,sys; assert json.load(sys.stdin)["enabled"] is True' || die "quota history endpoint"
curl -fsS -H "Authorization: Bearer $MGMT_KEY" "$BASE/v8/management/config" |
  python3 -c 'import json,sys; assert json.load(sys.stdin)["routing"]["strategy"] == "weekly-reset-weighted-round-robin"' ||
  die "routing strategy not accepted"
docker logs "$NAME" 2>&1 | grep -c 'quota history: recording' >/dev/null || die "collector did not start"
echo "smoke test passed"

cat >"$OUT/RELEASE.md" <<EOF
# Release $RELEASE

- Backend: akhial/CLIProxyAPI main @ $(git -C "$BACKEND" rev-parse --short main) ($VERSION) -> image \`$IMAGE\` (\`${IMAGE_ID:7:12}\`)
- UI: akhial/Cli-Proxy-API-Management-Center main @ $(git -C "$FRONTEND" rev-parse --short main) -> \`management.html\` ($UI_VERSION, sha256 \`$UI_SHA\`)
- Built and smoke-tested $(date -u +%Y-%m-%dT%H:%M:%SZ)

Deploy (check first, then for real):

\`\`\`sh
$BACKEND/fork/upgrade.sh --image $IMAGE --ui $OUT/management.html --check
$BACKEND/fork/upgrade.sh --image $IMAGE --ui $OUT/management.html
\`\`\`
EOF
say "release ready: $OUT"
cat "$OUT/RELEASE.md"
