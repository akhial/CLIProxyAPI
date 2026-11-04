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
| Artifact | Docker image `cli-proxy-api-akhial:<release>` (local, not pushed) | single-file `management.html` |

`main` in each fork is **an upstream release tag plus a short stack of fork
commits**. List them with `git log --oneline upstream/main..main`.

Backend stack:
1. `feat(management): record Claude/Codex quota history`: background collector,
   daily JSONL store, `GET /v8/management/quota/history`.
2. `feat(routing): add weekly-reset-weighted-round-robin strategy` and its
   follow-ups: weighted round-robin restricted to the available account with the
   earliest effective deadline (weekly reset, moved earlier for 5-hour-limited
   accounts under heavy traffic); see "Weekly-reset routing behaviour" below.
3. `docs(fork): …` commits: this `fork/` folder.
4. `feat(openai): provider-scoped Codex model catalog`: `GET /v1/models?provider=codex`
   (Codex client format) lists only Codex-registered models under their native
   names. Codex clients set `model_catalog_url` to that URL.

UI stack:
1. `style(shell): prox brand and neutral black dark theme`: almost all in `src/styles/fork.scss`.
2. `feat(quota): pool forecast, account grid and weekly-reset routing option`:
   also makes the Config and Routing pages know the fork strategy.
3. `docs(fork): note the fork and point to the playbook`: `FORK.md`.
4. `feat(dashboard): fork dashboard, plain loading screen, land on dashboard`:
   `src/features/dashboard/prox/`, `BootLoader`, `src/landOnDashboard.ts`, instant
   redirect after session restore.
5. Follow-up `fix(quota)`, `fix(dashboard)` and `feat(quota)` commits on the quota
   page, forecast and dashboard.

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
  standalone `internal/translator/` changes; UI: all seven locales, `bun run verify`).
- **Never delete, skip or weaken tests**, with one exception: an upstream test that
  only pins an artificial UI delay or animation timing (for example a minimum
  splash time) that contradicts fork behaviour. Keep the fork's behaviour and remove
  just that test case, together with the delay, in the fork commit that owns the
  behaviour; say so in its commit message.

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
   It builds `cli-proxy-api-akhial:<upstream-tag>-akhial.<backend sha>.<UI sha>` and the UI
   (a UI-only change is its own release; the server reports `<upstream-tag>-akhial.<backend sha>`). It then
   starts them in a throwaway container to check that the API answers, the UI is
   served, quota history works and the routing strategy is accepted. The result is
   `/srv/work/cliproxyapi-release/<release>/` with `management.html` and
   `RELEASE.md`, which contains the exact deploy commands.

5. **Deploy** (about 10 s of downtime). Run the check first:
   ```sh
   fork/upgrade.sh --image cli-proxy-api-akhial:<release> --ui /srv/work/cliproxyapi-release/<release>/management.html --check
   fork/upgrade.sh --image cli-proxy-api-akhial:<release> --ui /srv/work/cliproxyapi-release/<release>/management.html
   ```
   `upgrade.sh` preflights everything and backs up to
   `backups/pre-upgrade-<ts>/`. It then swaps the image and UI while the container
   is stopped and health-checks the result: API, image ID, served UI bytes, quota
   history, unchanged routing strategy, collector started. **If any check fails it
   restores the previous files and container automatically.** For a UI-only
   change, `sudo install -m 0644 <file> /srv/work/cliproxyapi/static/management.html`
   is enough; no restart is needed.

6. **Check it** in the UI (hard refresh): forecast charts render, account rows
   show "Recorded … ago", and both Config → Routing Strategy and the Routing page
   read "Reset weighted round-robin". Then run `docker compose logs --since 15m | grep -iE
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
leaves `main` and production untouched. Cosmetic UI timing (artificial delays,
splash duration, animations) is never a reason to stop: see the test rule under
*Rules*.

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
| `sdk/cliproxy/auth/selector.go` | `WeightedRoundRobinSelector.resetBoost` field. `Pick` calls `s.resetBoost.rankAuthWeights(weights, available)` before picking. `pickSmoothWeightedAuth` takes the effective `weights`. |
| `sdk/cliproxy/auth/scheduler.go` | Scheduler keeps `selectorResetBoost(selector)`. Every weighted pick path (single-provider, mixed-provider, websocket) ranks the weight vector before picking, and quota data comes from the newest auth snapshot. **Rule: wherever upstream computes weighted-round-robin weights, the ranking must be applied.** |
| `sdk/cliproxy/service_config.go` | `weekly-reset-weighted-round-robin` / `weekly-reset-wrr` in the strategy switch, built with `NewWeeklyResetWeightedRoundRobinSelector()`. |
| `internal/api/handlers/management/config_basic.go` | Same names in `normalizeRoutingStrategy`. |
| `internal/api/server.go` | `s.mgmt.StartQuotaHistory()` + `defer s.mgmt.StopQuotaHistory()` in `Start()`, before the serve goroutines, so the stored routing hints are loaded before the first request. |
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
| `src/components/layout/MainLayout.tsx` | Brand block uses `ForkBrandLogo` (upstream's `abbrBrandName` is dropped). Keep upstream's `arrivingFromLogin` reveal wiring. |
| `src/router/MainRoutes.tsx` | `/` and `/dashboard` render the fork's `ProxDashboardPage` (`src/features/dashboard/prox/`); upstream's `DashboardPage` is left in place, unrouted. |
| `src/pages/LoginPage.tsx`, `tests/autoLoginEntry.test.ts` | Session restore renders `BootLoader` (a word and a bar) instead of upstream's splash, and navigates as soon as `restoreSession()` succeeds: no `splashMin` hold, exit fade or reveal. Upstream's "splash pacing" test case that pins the minimum splash is removed. Manual sign-in keeps upstream's success → exit → reveal hand-off and its tests (`tests/loginTransition.test.ts`). |
| `src/pages/login/DotMatrixHero.tsx`, `tests/loginDotMatrixHero.test.ts` | The login hero drops upstream's `HERO_TITLE` tagline ("Welcome to CLI Proxy API."); the test checks that it is absent. Part of the `style(shell)` commit. |
| `src/main.tsx` | Imports `./landOnDashboard` first, so a fresh visit opens the dashboard before the hash router reads the URL. |
| `src/i18n/locales/*.json` (all seven, including `tr`) | `quota_forecast`, `quota_accounts` and `prox` blocks plus the strategy labels (`basic_settings`, `config_management.visual.sections.network` and `routing.strategy_weekly_reset_weighted_round_robin` with `_desc`). Git often misaligns the conflict hunks in these files, so keeping both text sides can corrupt them. Merge per key instead: take upstream's file and re-apply the keys the fork commit adds or removes. `tests/turkishLocale.test.ts` requires `tr.json` to have every `en.json` key at every commit. |
| `src/styles/global.scss`, `variables.scss` | `@use './fork.scss'` and `Inter` in the font stack. |
| `src/hooks/useVisualConfig.ts`, `src/types/visualConfig.ts`, `config/…/SectionNetwork.tsx`, `dashboard/DashboardPage.tsx`, `config/searchIndex.ts` | The new strategy value. Without it, saving the config resets it to round-robin. |
| `src/types/routing.ts`, `src/services/api/routing.ts`, `src/features/routing/logic.ts`, `routing/components/{PolicyPanel,MemberCard,WeightDialog}.tsx` | Routing page: `weekly-reset-weighted-round-robin` (and `weekly-reset-wrr`) is in `ROUTING_STRATEGIES` and `normalizeRoutingStrategy`, and counts as weighted through `isWeightedRoutingStrategy` (weight badges, weight dialog, traffic shares). Without it the page shows production's strategy as round-robin. `tests/routingWeeklyReset.test.ts` covers it. |
| `src/features/quota/QuotaPage.module.scss`, `src/services/api/index.ts` | Row skeleton style and the `quotaHistory` export. |
| `src/utils/quota/relativeTime.ts`, `tests/quotaRelativeTime.test.ts` | Days start at 48 h, so a reset 24–47 h away reads in hours and shows whether it is inside the 36 h priority window; the upstream test matches. |

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
- **UI routing strategy checks**: new upstream code that compares a strategy with
  `'weighted-round-robin'` must use `isWeightedRoutingStrategy`, and new strategy
  lists or normalizers must include `weekly-reset-weighted-round-robin`. Otherwise
  the UI shows production's strategy as round-robin, or saves it back as one.
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
- **Weekly-reset routing behaviour** (earliest effective deadline first; chosen by
  simulation against the best possible schedule, see `rankAuthWeights`):
  - Every pick goes to the available account with the earliest effective deadline:
    its weekly reset, moved earlier for a 5-hour-limited account by the extra time
    its windows add to spending the rest of its week at the pool's 1-day average
    traffic. Under light traffic that is nothing (plain earliest reset first); under
    heavy traffic a limited account starts early enough to use its windows in time.
  - Within `WeeklyResetTieBand` (1 h) of the earliest deadline, limited accounts go
    first. Exact ties share by configured weight; unknown resets rank last. A passed
    reset counts as the next week's, with nothing used.
  - When the chosen account cools down (5-hour limit, 429), the next deadline serves
    until it is back.
  - The collector (`quota_history_routing.go`) measures every 10 min, from the last
    7 days of history: each account's weekly share of one full 5-hour window (median
    of windows that ended in the last 48 h; Anthropic changed the ratio on
    2026-11-03) and the pool's traffic over the last 24 h, converted to each
    account's %/h by plan size (Pro 1, Max 5, Ultra 10). `SetRoutingStats`.
  - Weekly and 5-hour usage come from the unified `7d`/`5h` headers and the
    collector's samples (`SetWeeklyQuotaHint`, `SetSessionQuotaHint`), newest wins.
    An account counts as limited until one of them has seen it without the 5-hour
    window. On startup, `StartQuotaHistory` republishes the hints of the last 48 h of
    samples and measures the stats before the server accepts connections.
  - `GET /v8/management/quota/history` returns each account's `routing` (deadline,
    session_limited, session_share, drain); the UI's Priority badge and forecast use it.
  - An exhausted account keeps its deadline on paper, but the proxy benches it after
    its first 429 until the weekly reset (cooldown taken from Anthropic's
    `7d-Reset`), so traffic goes elsewhere.
  - Cooldowns are kept in memory unless `routing.cooldown.save-cooldown-status:
    true`. After a restart, each exhausted account ranked first therefore costs one
    failed attempt before it's benched again (the request is retried on another
    account).
  - A manual reset (Claude banked reset, Codex manual reset) moves the reset time;
    the ranking corrects on the next response or poll, within ≤10 min.
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
