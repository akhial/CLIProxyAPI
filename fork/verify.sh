#!/usr/bin/env bash
# Run the fork's checks on a backend or frontend checkout.
#
# Usage: fork/verify.sh <repo-dir> [base-ref]
#   base-ref: upstream commit the fork sits on (default: merge-base with upstream/main);
#             only Go files changed since it are gofmt-checked.
set -euo pipefail

REPO=${1:?usage: verify.sh <repo-dir> [base-ref]}
cd "$REPO"
BASE=${2:-$(git merge-base HEAD upstream/main)}

if [[ -f go.mod ]]; then
  mapfile -t changed_go < <(git diff --name-only --diff-filter=d "$BASE" HEAD -- '*.go')
  if ((${#changed_go[@]})); then
    unformatted=$(gofmt -l "${changed_go[@]}")
    [[ -z $unformatted ]] || { echo "gofmt needed: $unformatted" >&2; exit 1; }
  fi
  go vet ./sdk/cliproxy/... ./sdk/api/handlers/openai/ ./internal/api/... ./internal/config/...
  go test -count=1 ./sdk/cliproxy/auth/... ./internal/api/... ./internal/config/...
  # Only the catalog tests: the package also holds a timing-sensitive upstream test.
  go test -count=1 -run 'ModelsProviderCatalog|CodexClientModels' ./sdk/api/handlers/openai/
  # Upstream's sdk/cliproxy package has timing-sensitive tests; retry once before failing.
  go test -count=1 ./sdk/cliproxy/ || go test -count=1 ./sdk/cliproxy/
  go build -o /tmp/cpa-verify-check ./cmd/server
  rm -f /tmp/cpa-verify-check
else
  bun install --frozen-lockfile
  bun run verify
fi
echo "verify: all checks passed in $REPO"
