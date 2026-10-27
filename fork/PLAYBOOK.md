# akhial fork maintenance playbook

How to keep the two forks current with upstream and ship them to the
production proxy on `baghdad`.

## What the fork is

| | Backend | Management UI |
|---|---|---|
| Fork | github.com/akhial/CLIProxyAPI | github.com/akhial/Cli-Proxy-API-Management-Center |
| Upstream | router-for-me/CLIProxyAPI | router-for-me/Cli-Proxy-API-Management-Center |
| Checkout | `/srv/work/code/akhial/CLIProxyAPI` | `/srv/work/code/akhial/Cli-Proxy-API-Management-Center` |
| Remotes | `origin` = fork, `upstream` = upstream | same |
| Artifact | Docker image `cli-proxy-api-akhial:<version>` (local, not pushed) | single-file `management.html` |

`main` in each fork is **an upstream release tag plus a short stack of fork
commits**. List them with `git log --oneline upstream/main..main`.

Backend stack:
1. `feat(management): record Claude/Codex quota history`: background collector,
   daily JSONL store, `GET /v8/management/quota/history`.
2. `feat(routing): add weekly-reset-weighted-round-robin strategy`: weighted
   round-robin; accounts whose weekly window resets in <36h get weight 1,000,000.
3. `docs(fork): …` commits: this `fork/` folder.
4. `feat(openai): provider-scoped Codex model catalog`: `GET /v1/models?provider=codex`
   (Codex client format) lists only Codex-registered models under their native
   names. Codex clients set `model_catalog_url` to that URL.

UI stack:
1. `style(shell): prox brand and neutral black dark theme`: almost all in `src/styles/fork.scss`.
2. `feat(quota): pool forecast, account grid and weekly-reset routing option`.
3. `docs(fork): note the fork and point to the playbook`: `FORK.md`.
4. `feat(dashboard): fork dashboard, plain loading screen, land on dashboard`:
   `src/features/dashboard/prox/`, `BootLoader`, `src/landOnDashboard.ts`.

Production: `/srv/work/cliproxyapi` (Docker Compose, Tailscale Serve on :8317).
Its `README.md` records the running version and the latest backup.

## Rules

- **Sync to upstream release tags**, not arbitrary upstream commits. One sync per
  week, plus emergency syncs for security or provider breakage.
- **Rebase, never merge upstream into `main`.** A sync replays the fork commits on
  the new tag, so `main` is rewritten: publish with `--force-with-lease`. The
  script keeps a `backup/main-<date>-pre-<tag>` branch locally.
- **Keep the stack small.** One commit per feature. Prefer new files to edits of
  upstream files; keep fork styling in `fork.scss`. When changing a fork feature,
  amend its commit (`git commit --fixup <sha>` + `git rebase --autosquash`) rather
  than adding a new one.
- **Never push tags to the forks.** Upstream's tag-triggered workflows (`release.yaml`,
  `docker-image.yml`, UI `release.yml`) would try to publish releases and Docker
  images. GitHub Actions are currently not enabled on either fork, so there is no
  CI: `fork/verify.sh` is the gate.
- Follow each repo's `AGENTS.md` (backend: gofmt, no `/v0/management` changes, no
  standalone `internal/translator/` changes; UI: all six locales, `bun run verify`).

## Weekly routine

Allow 30–60 minutes when nothing conflicts.

1. **Read what changed.** Open the upstream releases since the last sync
   ([backend](https://github.com/router-for-me/CLIProxyAPI/releases),
   [UI](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases)).
   Look for the areas listed under *Watch list* below.

2. **Sync the backend.**
   ```sh
   cd /srv/work/code/akhial/CLIProxyAPI
   fork/sync-upstream.sh .            # or: fork/sync-upstream.sh . v8.0.50
   ```
   It fetches, prints the upstream files the fork also edits, rebases onto the
   newest stable upstream release on branch `sync/<tag>`, then runs `fork/verify.sh`
   (gofmt, vet, tests, build). On success it prints the publish command:
   ```sh
   git push --force-with-lease=main:<old-sha> origin sync/<tag>:main
   git switch main && git reset --hard origin/main
   ```
   On a conflict it stops with exit code 2: see *Conflict hotspots*, then
   `git add … && git rebase --continue`, then `fork/verify.sh . <tag>`, then publish.

3. **Sync the UI** the same way:
   `fork/sync-upstream.sh /srv/work/code/akhial/Cli-Proxy-API-Management-Center`
   (the checks are `bun install --frozen-lockfile` and `bun run verify`).

4. **Build and smoke-test a release** from both `main` branches:
   ```sh
   fork/build-release.sh
   ```
   It builds `cli-proxy-api-akhial:<upstream-tag>-akhial.<sha>` and the UI. It then
   starts them in a throwaway container to check that the API answers, the UI is
   served, quota history works and the routing strategy is accepted. The result is
   `/srv/work/cliproxyapi-release/<version>/` with `management.html` and
   `RELEASE.md`, which contains the exact deploy commands.

5. **Deploy** (about 10 s of downtime). Run the check first:
   ```sh
   fork/upgrade.sh --image cli-proxy-api-akhial:<version> --ui /srv/work/cliproxyapi-release/<version>/management.html --check
   fork/upgrade.sh --image cli-proxy-api-akhial:<version> --ui /srv/work/cliproxyapi-release/<version>/management.html
   ```
   `upgrade.sh` preflights everything and backs up to
   `backups/pre-upgrade-<ts>/`. It then swaps the image and UI while the container
   is stopped and health-checks the result: API, image ID, served UI bytes, quota
   history, unchanged routing strategy, collector started. **If any check fails it
   restores the previous files and container automatically.** For a UI-only
   change, `sudo install -m 0644 <file> /srv/work/cliproxyapi/static/management.html`
   is enough; no restart is needed.

6. **Check it** in the UI (hard refresh): forecast charts render, account rows
   show "Recorded … ago", and Config → Routing Strategy reads "Weekly reset
   weighted round-robin". Then run `docker compose logs --since 15m | grep -iE
   'panic|quota poll failed'` in `/srv/work/cliproxyapi`.

7. **Record it**: update the `Version:` paragraph in
   `/srv/work/cliproxyapi/README.md`, and prune old images (keep the running one
   and the previous one): `docker image ls cli-proxy-api-akhial`.

## Automation

A T3 scheduled agent task, "Weekly CLIProxyAPI fork sync & redeploy", runs this
routine every **Monday at 03:00 UTC** (host time). For each repo it:

1. Picks the newest stable upstream release.
2. Rebases and fixes conflicts or fork-caused failures, keeping the fixes inside
   the fork commits they belong to.
3. Verifies, then publishes `main`.
4. Builds and smoke-tests a release, then deploys it with `upgrade.sh`.

It writes a report to a new thread on every run.

**It deploys nothing when** the preflight fails, a conflict or failure can't be
fixed without changing fork behaviour, or checks still fail. In those cases it
pushes the work in progress to `origin` as branch `sync/<tag>` for review, and
leaves `main` and production untouched.

Pause or change it from T3's scheduled tasks. If you fix things by hand, do it on
`main` before the next run: the task refuses to start when local `main` differs
from `origin/main` or the tree is dirty.

## Conflict hotspots

These are the upstream files the fork edits. New fork files never conflict.
When resolving, **take upstream's version and re-apply the fork's intent**
described here.

Backend:

| File | Fork change |
|---|---|
| `sdk/cliproxy/auth/selector.go` | `WeightedRoundRobinSelector.resetBoost` field. `Pick` calls `s.resetBoost.boostAuthWeights(weights, available)` before picking. `pickSmoothWeightedAuth` takes the effective `weights`. |
| `sdk/cliproxy/auth/scheduler.go` | Scheduler keeps `selectorResetBoost(selector)`. Every weighted pick path (single-provider, mixed-provider, websocket) boosts the weight vector before picking, and quota data comes from the newest auth snapshot. **Rule: wherever upstream computes weighted-round-robin weights, the boost must be applied.** |
| `sdk/cliproxy/service_config.go` | `weekly-reset-weighted-round-robin` / `weekly-reset-wrr` in the strategy switch, built with `NewWeeklyResetWeightedRoundRobinSelector()`. |
| `internal/api/handlers/management/config_basic.go` | Same names in `normalizeRoutingStrategy`. |
| `internal/api/server.go` | `s.mgmt.StartQuotaHistory()` + `defer s.mgmt.StopQuotaHistory()` in `Start()`. |
| `internal/api/server_management_v8.go` | `v8.GET("/quota/history", s.mgmt.GetQuotaHistory)`. |
| `internal/config/config_types.go`, `config.example.yaml` | Docs for the new strategy only. |
| `sdk/cliproxy/auth/selector_test.go` | One call site uses the new `pickSmoothWeightedAuth` signature. |
| `sdk/api/handlers/openai/openai_handlers.go` | In `OpenAIModels`, the Codex-client branch passes the response through `filterCodexClientModelsByProvider` when `?provider=` is set. |
| `sdk/api/handlers/openai/codex_client_models.go`, `…_test.go` | `filterCodexClientModelsByProvider` and `TestOpenAIModelsProviderCatalog`. |

UI:

| File | Fork change |
|---|---|
| `src/features/quota/QuotaPage.tsx` | Forecast section under the tabs, `useForecastPrefs`, history shared with `AccountsTable` (which replaced the card grid), plugin tab hidden when empty, masked names. |
| `src/features/quota/components/QuotaHeader.*` | "Show emails" button and the inline header style. |
| `src/features/authFiles/components/ProviderTabs.*` | `variant="pills"`. |
| `src/components/layout/MainLayout.tsx` | Brand block uses `ForkBrandLogo`. |
| `src/router/MainRoutes.tsx` | `/` and `/dashboard` render the fork's `ProxDashboardPage` (`src/features/dashboard/prox/`); upstream's `DashboardPage` is left in place, unrouted. |
| `src/pages/LoginPage.tsx`, `src/router/ProtectedRoute.tsx` | Session restore shows `BootLoader` (a word and a bar) instead of the splash/spinner; auto-login redirects without the 1.5 s pause. |
| `src/main.tsx` | Imports `./landOnDashboard` first, so a fresh visit opens the dashboard before the hash router reads the URL. |
| `src/i18n/locales/*.json` (all six) | `quota_forecast`, `quota_accounts` and `prox` blocks plus two routing-strategy labels. Keep both sides of adjacent-key conflicts; the build fails on invalid JSON. |
| `src/styles/global.scss`, `variables.scss` | `@use './fork.scss'` and `Inter` in the font stack. |
| `src/hooks/useVisualConfig.ts`, `src/types/visualConfig.ts`, `config/…/SectionNetwork.tsx`, `dashboard/DashboardPage.tsx`, `config/searchIndex.ts` | The new strategy value. Without it, saving the config resets it to round-robin. |
| `src/features/quota/QuotaPage.module.scss`, `src/services/api/index.ts` | Row skeleton style and the `quotaHistory` export. |

## Watch list in upstream release notes

Even a clean rebase can break behaviour in these areas. Read the diffs, and if they
change, re-check the fork feature that depends on them:

- **Routing internals**: `selector.go`, `scheduler.go`, session affinity
  (`conductor_selection.go`), weight parsing (`internal/credentialweight`). Boost
  wiring could silently stop applying; `weekly_reset_boost_test.go` must still pass.
- **Quota headers**: `quota_signals.go`, `claude_ratelimit.go`, `codex_quota.go`
  (header names and formats feed the collector, the boost and the dashboard's account rows).
- **Credentials list shape**: `recent_requests` (20 epoch-aligned 10-minute buckets),
  `quota.signals`, `cooldowns` and `observed_at` in `GET /v8/management/credentials`
  feed the UI dashboard (`src/features/dashboard/prox/model.ts`).
- **Management plumbing** the collector reuses: `resolveTokenForAuth`,
  `apiCallTransport`, `Handler`, `Server.Start/Stop`.
- **Provider usage APIs**: if upstream changes `CLAUDE_USAGE_URL`, `CODEX_USAGE_URL`
  or their headers in the UI's `src/utils/quota/constants.ts`, mirror that in
  `quota_history_collector.go`.
- **UI quota providers**: state shapes in `src/types/quota.ts` and
  `providers/*/data.ts` feed `accounts/rowModel.ts`. `QuotaCard.tsx` changes (new
  actions) may need the same change in `accounts/AccountRow.tsx`, which replaced it.
- **Codex model catalog**: `codex_client_models.go`, `registry.GetCodexClientModelsJSON`
  and `GetModelProviders`. If upstream adds its own provider filter, prefer it and
  keep the `provider=codex` URL working for existing Codex clients.
- **Upstream ships the same thing** (quota history, a reset-aware strategy): prefer
  upstream's version and drop or slim down the fork commit.

## Emergencies

- **Upstream security or provider fix**: run the weekly routine immediately for the
  new tag.
- **Fork can't be rebased in time**: production can run the official image
  temporarily. First set `routing.strategy` to `weighted-round-robin`, because the
  official build doesn't know the fork strategy and would fall back to round-robin.
  The fork UI shows a "history unavailable" notice on the official backend; restore
  the official UI from a backup if needed. Keep `disable-auto-update-panel: true`
  either way.

## Rollback

`upgrade.sh` rolls back on its own. To go back manually to any earlier state:

```sh
cd /srv/work/cliproxyapi
docker compose stop
cp -p backups/<backup-dir>/compose.yaml compose.yaml
cp -p backups/<backup-dir>/config.yaml config/config.yaml
cp -p backups/<backup-dir>/management.html static/management.html
docker compose up -d --pull never
```

Older images stay local until pruned, so rolling back the image is instant.

## Runtime notes

- `management.disable-auto-update-panel: true` must stay set. Otherwise the
  backend downloads the official UI over the fork UI every few hours.
- Quota history lives in `config/quota-history/` (root-owned, 35-day retention).
  Env knobs: `QUOTA_HISTORY_DISABLE`, `QUOTA_HISTORY_DIR`,
  `QUOTA_HISTORY_POLL_INTERVAL` (default `10m`; `0` = header-only).
- **Weekly-reset boost behaviour**:
  - The boost holds only while `0 < reset - now < 36h`, so it ends exactly at the
    reset. The next poll (≤10 min) or response then records the new reset ~7 days
    out.
  - An exhausted account keeps the boost on paper, but the proxy benches it after
    its first 429 until the weekly reset (cooldown taken from Anthropic's
    `7d-Reset`), so traffic goes elsewhere.
  - Cooldowns are kept in memory unless `routing.cooldown.save-cooldown-status:
    true`. After a restart, each exhausted-but-boosted account therefore costs one
    failed attempt before it's benched again (the request is retried on another
    account).
  - A manual reset (Claude banked reset, Codex manual reset) moves the reset time;
    the boost corrects on the next response or poll, within ≤10 min.
- Upstream's `sdk/cliproxy` tests `TestHandleAuthUpdates_SameRevisionWait…`,
  `TestEndToEndAuthFileReplacement…` and `TestServiceHeartbeatFailover…` are
  timing-sensitive and flake on unmodified upstream too; `verify.sh` retries that
  package once.

## Scripts

| Script | Purpose |
|---|---|
| `fork/sync-upstream.sh <repo> [tag]` | Rebase the fork stack onto an upstream tag on `sync/<tag>`, then verify. |
| `fork/verify.sh <repo> [base]` | The checks: backend gofmt, vet, tests, build; UI `bun run verify`. |
| `fork/build-release.sh [ui-repo]` | Build the image and UI from both `main` branches and smoke-test them. |
| `fork/upgrade.sh --image … [--ui …] [--check]` | Atomic production upgrade with automatic rollback. For rehearsals against a copy, set `PROD`, `CONTAINER`, `BASE` and `PUBLIC`. |
