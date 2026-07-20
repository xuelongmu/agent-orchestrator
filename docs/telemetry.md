# Telemetry

AO uses anonymous telemetry to understand reliability and product usage. The
Electron renderer sends sanitized PostHog events directly, and the Go daemon can
persist allowlisted events locally and fan them out to PostHog when remote
telemetry is enabled.

## What is collected

- App activation events: `ao.app.active` from the renderer and CLI
- Renderer load and route views, grouped by coarse surface names
- Project/task/session UI actions, with project identifiers SHA-256 hashed
- Renderer exceptions, reduced to error name and coarse context
- Daemon operational events: CLI invocation, session spawn/failure, waiting-input
  transitions, lifecycle poll duration/overruns, HTTP 5xx, and daemon panics
- AO version context (`app_version` / `ao_version`), platform, and build mode

PostHog session recording is enabled for the renderer. Network request names are
masked before recording.

## Privacy

Before any renderer event or recording is transmitted:

- Absolute file paths (`/home/...`, `/Users/...`, `C:\...`) are replaced with
  `[redacted-local-path]`
- Local URLs (`file://`, `app://renderer`, `localhost`, `127.0.0.1`, `[::1]`)
  are replaced with `[redacted-local-url]`
- Project IDs are one-way hashed and never sent in plain text

Daemon events use a remote payload allowlist before PostHog export. Project and
session IDs are hashed, and raw location/IP fields are not accepted from AO
payloads. Geographic reporting should use PostHog's GeoIP enrichment only.

All daemon events set PostHog's `$process_person_profile` to `false`. The
renderer initializes PostHog with `person_profiles: "never"`, bootstraps the
install ID as anonymous, and never calls `identify()`. On upgrade, AO removes
legacy PostHog persistence that records an identified user before PostHog is
initialized. The random install ID can group anonymous activity from one
installation, but AO does not create or retain PostHog person profiles.

## Remote volume controls

Volume controls apply only to the billed remote branch. They never filter the
local SQLite branch, which receives each raw daemon event—including repeated
CLI commands and every lifecycle poll—through the existing best-effort sink
and normal 30-day retention policy.

- `ao.http.5xx`, `ao.daemon.panic`, and `ao.cli.usage_errors` are aggregated
  into one remote rollup per rolling minute. The rollup includes `count`,
  `window_start`, and `window_end`; its other dimensions come from the most
  recent event in that window.
- Healthy `ao.lifecycle.poll` completions are sampled once per fixed 15-minute
  UTC bucket (96 samples for a continuously running full day). Every failed or
  overrun poll bypasses sampling and rate limits so health regressions remain
  visible. Healthy samples use a deterministic PostHog `$insert_id`, so a
  daemon restart in the same bucket retries the same provider event rather
  than creating a new one.
- Remote daemon events also have a process-local guard of five accepted events
  per name per rolling minute and 200 per name per rolling 24 hours. Aggregated
  names use a 1,500-per-24-hour backstop because aggregation already removes
  per-occurrence cost. These are deliberately described as process-local cost
  guards: restarting the daemon resets them, so they are not durable daily
  ceilings.
- Repeated `ao.cli.invoked` events are reduced remotely to one event per
  command path and UTC day, while CLI `ao.app.active` is reduced to one event
  per UTC day. Their `$insert_id` values are derived from the anonymous install
  ID and UTC-day key, making retries stable across daemon restarts. The HTTP
  route still emits every invocation into the fanout; remote deduplication
  never removes entries from the local SQLite branch.
- Renderer captures have a process-local five-per-name rolling-minute burst
  guard and a 200-per-name UTC-day budget stored in `localStorage`. The daily
  budget therefore survives renderer reloads. If browser storage is
  unavailable, the fallback is process-local and is not a durable ceiling.

## Install ID

On first run, a random install identifier is generated and stored at
`~/.ao/data/telemetry_install_id` (or `$AO_DATA_DIR/telemetry_install_id`). The
renderer and daemon both use this ID as the PostHog distinct ID so activity is
grouped across app launches and CLI invocations. It is not linked to any
personal account and is never promoted to a PostHog person profile.

## Configuration

Renderer PostHog key and host are baked in at build time. To point a build at
another PostHog project, set these environment variables before building:

```bash
VITE_AO_POSTHOG_KEY=phc_yourkey
VITE_AO_POSTHOG_HOST=https://your-posthog-host.com
```

Daemon event capture is off by default when the daemon is launched directly. The
Electron supervisor starts the daemon with these defaults unless the environment
already provides explicit values:

```bash
AO_TELEMETRY_EVENTS=on
AO_TELEMETRY_REMOTE=posthog
AO_TELEMETRY_POSTHOG_KEY=phc_yourkey
AO_TELEMETRY_POSTHOG_HOST=https://us.i.posthog.com
```

Local daemon telemetry is retained in SQLite for 30 days.

Simplification-round telemetry uses a stronger local boundary than ordinary
best-effort events. AO transactionally inserts the local telemetry row and
stamps the review-run receipt under one deterministic event ID derived from the
review run and exact target SHA. The still-undelivered review run retries fanout
after a crash; SQLite ignores the repeated ID and PostHog receives the same
`$insert_id` for provider deduplication. The local SQLite row is therefore the
exactly-once system of record. Remote export remains buffered and best-effort:
AO can make retries idempotent, but cannot guarantee that an external provider
accepts an event before the review run is ultimately stamped delivered. The
30-day retention job does not prune a referenced simplification intent while
its review run remains undelivered; after delivery, normal retention applies.

## PostHog Retention And Geography Dashboard

Use `ao.app.active` as the active-user event for DAU, weekly retention, and
country-level active-user maps. AO emits it from:

- `channel=renderer` when the desktop app initializes and at most once per UTC
  day while the app stays open
- `channel=cli` when the CLI reports a command invocation to the local daemon
  (remote export uses one deterministic UTC-day event; local history keeps each
  invocation)

Recommended PostHog setup:

1. Enable PostHog GeoIP enrichment for the project.
2. Create an "AO Active Users" dashboard.
3. Add a Trends insight:
   - Event: `ao.app.active`
   - Aggregation: unique users
   - Chart type: world map
   - Breakdown: GeoIP country code, for example `$geoip_country_code`
4. Add a Retention insight:
   - Start event: `ao.app.active`
   - Return event: `ao.app.active`
   - Interval: weekly
   - Range: last 12 weeks
5. Add optional filters or breakdowns for `channel=renderer` and `channel=cli`
   when comparing desktop app and CLI activity.

PostHog references:

- GeoIP enrichment: https://posthog.com/docs/cdp/geoip-enrichment
- Trends insights: https://posthog.com/docs/product-analytics/trends
- Retention insights: https://posthog.com/docs/product-analytics/retention
