# CLAUDE.md — mac-fleet

Central control plane for a fleet of bare-metal macOS hosts (Nomad client + Tart VMs, both run as a non-root user). See README.md for user-facing docs.

## Problem being solved
1. No observability of Mac hosts (CPU/memory/disk/etc).
2. SSH needed for every Nomad client op (drain, stop, restart, config update).
3. SSH needed for Tart ops (list / delete / pull images).

Solution: a server on Kubernetes plus an agent on each Mac. Agents dial out; no inbound access or SSH to the Macs.

## Decisions made by the user (do not revisit without asking)
- Traffic is over the public internet; firewall rules will be added later.
- Tart images are public DockerHub images.
- Server holds the Nomad ACL token, editable from the dashboard (Settings).
- No auth/ACL on the dashboard for now (**public Ingress is therefore a risk; restrict before real use**).
- Deploy on Kubernetes with Postgres; no heavy Grafana. Dashboard shows live data and charts, 30-day retention.
- Minimal **light** theme.
- 

## Architecture
- Go 1.24, module `github.com/harness/mac-fleet`. Deps: pgx/v5, gopsutil/v4, gorilla/websocket.
- `cmd/server`, `cmd/agent` (subcommands: enroll, run, install-launchd, version).
- Transport: **WebSocket over mTLS** (not gRPC) on agent listener `:8443`; dashboard/API on `:8080`.
- Enrollment: one-time token + SHA-256 CA pin (`--ca-sha256`); server runs its own CA; per-host client cert (CN = host ID, 90 days, auto-renew).
- All Nomad operations run on a Mac agent (the server usually can't reach a VPN-only Nomad API). Drain/eligibility/ping use the agent's local Nomad client API with the cluster token (AES-GCM in Postgres, key = `MASTER_KEY`), attached at send time and never stored in `commands.params`. `pickExecutor` prefers the target host, else any connected host in the same cluster (event line says which); none online -> fails fast. Restart/stop/start/config are always on the target host. The server also pushes the host's cluster token to its agent (`settings` message after hello and on cluster/assignment changes); the agent keeps it in memory only and uses it to read node eligibility/drain state. Known gap: if the executor drops mid-drain the command stays running until the janitor fails it.
- Multiple Nomad clusters: `clusters` table (name, addr, tls_server_name) with token/CA/client cert/key stored encrypted in `settings` as `cluster:<id>:<part>`; `hosts.cluster_id` ('' = none). Drain/eligibility use the host's cluster (`clusterOf`; the only cluster if there is one); Test connection tries direct, then via a Mac. Blast-radius is checked per cluster. Enrollment tokens may carry a cluster (label `cluster`, moved to the column on enroll). The dashboard header switcher filters hosts/images client-side (`inCluster`) and `?cluster=` on `/api/images`. `migrateClusters` converts the old single `nomad_addr`/`nomad_token` into a cluster named `default` on startup.
- Nomad on a host is found from the running `nomad agent` process (`ps`): its `-config` file gives the API address, TLS (CA from `ca_file`, chain verified, host name not), and the config path. Nothing is hardcoded; agent config `nomad_addr`/`nomad_config`/`nomad_label` override.
- Restart/stop/start use launchctl (`kickstart -k`, `bootout`, `bootstrap`) on the job `nomad_label` (default `io.harness.nomad`). If Nomad was started from a shell the agent refuses and the UI offers `nomad.service.install` ("Manage with launchd"): it writes a plist with the same argv and PATH, SIGTERMs the old pid (never SIGKILL), bootstraps, waits ready, and restarts the original command if that fails.
- Config: `nomad.config.read` returns the file via `wire.Result.Data` (cached encrypted in `secrets` as `nomadcfg:<host>`); `nomad.config.apply` validates with the running nomad binary, backs up to `~/.macfleet/backups` (last 10), writes in place (keeps owner/mode) or atomically, restarts via launchd if managed (skipped with a warning otherwise), rolls back if not ready. `validate_only` checks without saving.
- Hosts report IPs (`Hello.IPs`) and the server stores the connection's remote IP; Tart list errors are sent as `Inventory.Error` and shown on the host page. Tart/nomad binaries are found with PATH fallbacks because launchd's PATH is minimal.
- Safety: typed allowlisted commands only, image-name regex validation (server and agent), one serial command worker per agent, commands queue while offline (1h expiry), blast-radius limit (`max_out_pct` default 10, `override_limit`), batches in waves that halt at `fail_limit`, audit log.
- Metrics: Postgres only. Day-partitioned `metrics_raw` (2 days), `metrics_5m` rollups (30 days). Maintenance loop (every 5 min) creates partitions, rolls up, drops old. SSE (`/api/events`) for live UI. Alerts derived from latest sample + thresholds.
- UI: vanilla JS embedded with `go:embed` in `internal/server/web` (index.html, style.css, app.js).
- Key routes: GET /api/hosts, /api/hosts/{id}, /api/hosts/{id}/metrics?range=1h|24h|7d|30d, POST /api/hosts/{id}/commands, /revoke, GET /api/commands, POST/GET /api/batches, POST /api/batches/{id}/cancel, GET /api/images, GET/PUT /api/settings, POST /api/settings/nomad/test, POST /api/enroll-tokens, GET /api/audit, /healthz, /readyz. Agent listener: /v1/enroll, /v1/renew, /v1/agent (WS).
- Deploy: `deploy/k8s/base` and `deploy/k8s/overlays/example` (copy to git-ignored `overlays/local` for real values) (namespace, server Deployment 1 replica Recreate, Postgres StatefulSet 20Gi, web Service + GCE Ingress/ManagedCertificate, agent Service type LoadBalancer TCP 8443, nightly pg_dump CronJob, NetworkPolicy). Env names: DATABASE_URL, MASTER_KEY, AGENT_HOSTS, PUBLIC_AGENT_URL, HTTP_ADDR, AGENT_ADDR, RAW_RETENTION_DAYS, RETENTION_DAYS, AGENT_CERT_DAYS.

## Status
- Server, agent and web UI written; `go vet` clean.
- Smoke-tested end to end against fake Nomad/Tart and a temp Postgres: enroll, mTLS WS, metrics, inventory, tart.pull, drain via Nomad API, config apply + `.bak`, blast-radius refusal/override, settings token, batch rollout, enrollment-token reuse rejected (401), bad image rejected.
- Dockerfile, Makefile, Kustomize manifests, README written.
- **NOT done: deployment.** The Claude cloud session had an invalid gcloud token and the device sandbox has no gcloud. Nothing has been created in GCP.
- NOT verified: manifests applied to a real cluster; browser rendering of the web UI; real `tart list --format json` field names (parser is tolerant: Name/Source/Size/Disk/Running/State/Accessed); `nomad.service.install` and launchctl restart on a real Mac (only unit-tested for plist/HCL/ps parsing; fake-Nomad TLS discovery tested on Linux); cert renewal; 5-minute rollup over real time; Nomad API reachable from the GKE cluster.

## Status and next steps
Deployment specifics (cluster, project, image paths, handoff checklist) are in `CLAUDE.local.md`, which is git-ignored. Read it first if present. Generic outline: publish a release tag, deploy the server with the local overlay, install the agent on one Mac from the dashboard, validate (tart list parsing, Nomad launchd label), then roll out in waves.

## Conventions
- Keep commands typed and allowlisted; never add a generic shell-exec command.
- Validate inputs on both server and agent.
- Light theme only; palette variables are in `internal/server/web/style.css`.
- `go build ./... && go vet ./...` before committing.

## UI architecture (updated 2026-10-30)
- `internal/server/web/app.js` keeps a local store (`S.hosts`) and patches DOM nodes in place; it never rebuilds a whole page on updates. `syncList` keys children, `setNum` animates numbers, `Chart` draws live SVG charts.
- SSE `host` events now carry the full host view (`publishHost` in hosts.go), so the browser needs no refetch. Other events (`command`, `batch`, `images`) trigger small debounced refetches.
- Hosts that go offline produce no event, so the client resyncs `/api/hosts` every 20s and on SSE reconnect or tab focus.
- Static files are served with content-hash ETags (`staticHandler` in server.go).
- Local UI test: simulation script idea is Postgres + server + several agents with `metrics_every_sec: 3`; headless Playwright can drive it.
- Redeploy after UI changes: `gcloud builds submit --tag <image>:<tag> --project PROJECT_ID`, then `kubectl -n macfleet set image deploy/macfleet-server server=<image>:<tag>` (or `make deploy TAG=<tag>`).

## Releases (added 2026-10-30)
- Repo: raghavharness/mac-fleet (public). `make release TAG=vX.Y.Z` publishes (or, if `.github/workflows/` is tracked, pushing the tag triggers `release.yml`, which publishes) `macfleet-agent-darwin-{arm64,amd64}`, `install.sh` and `SHA256SUMS`.
- `scripts/install.sh` installs/upgrades and enrolls an agent on a Mac (public repo: plain release URLs, no token; with `GITHUB_TOKEN` set it uses the API and parses JSON with macOS `osascript -l JavaScript`). It has NOT been run on a real Mac yet.
- Dashboard Settings → "Install the agent on a Mac" shows the full flow; API: GET /api/agent-install, POST /api/enroll-tokens returns `install_command`.
- Module path is github.com/raghavharness/mac-fleet.
- Agent self-upgrade (`agent.upgrade`, internal/agent/upgrade.go, internal/server/release.go): the server reads the GitHub release (`AGENT_RELEASE_REPO`, public only) and its SHA256SUMS, puts the URL and checksum per GOOS-GOARCH in the command params (fixed once per batch); the agent downloads, verifies the checksum, runs `new version`, keeps `<exe>.prev`, writes `~/.macfleet/upgrade.json`, swaps the binary and `launchctl kickstart -k`s itself. The new process removes the marker after hello; after 3 failed starts or 3 minutes without connecting it restores `.prev` and exits so launchd starts the old one. Needs an agent that already knows the command, so the first move to it is a manual `install.sh --upgrade`. Refuses when not under launchd.
