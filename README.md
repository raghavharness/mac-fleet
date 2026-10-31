# mac-fleet

Central control plane for a fleet of bare-metal macOS hosts running a Nomad client and Tart VMs.

- **Server** (runs on Kubernetes, for example GKE): dashboard, API, Postgres storage, CA.
- **Agent** (runs on every Mac as a launchd service, non-root): collects metrics, runs allowlisted Nomad/Tart operations.

Agents dial out to the server. No inbound ports or SSH are needed on the Macs.

## What it does

| Problem | Feature |
|---|---|
| No observability | CPU, memory, disk, load, VM count, Nomad state per host; live via SSE; charts 1h/24h/7d/30d (5-min rollups, 30-day retention); threshold alerts |
| SSH for Nomad ops | Drain / eligibility run through a Mac agent against the cluster's Nomad API (ACL token held by the server and sent with the command only; falls back to any other online Mac in the same cluster when the target is down); restart / stop / start through launchctl, an in-dashboard Nomad config viewer/editor (validate, diff, backup, rollback), and "Manage with launchd" to take over a Nomad that was started from a shell. Nomad's address, TLS and config path are discovered from the running process |
| Several Nomad environments | Add any number of Nomad clusters (address, ACL token, optional CA / client cert / key files); each host belongs to one, the header switcher scopes the dashboard, and limits apply per cluster |
| Finding a host | Host IPs shown and searchable |
| SSH for Tart ops | List / pull (public DockerHub) / delete images, per host or as a wave rollout across the fleet |

Safety: typed allowlisted commands only (no shell passthrough), image-name validation, commands queue while a host is offline (1h expiry), blast-radius limit (default 10% of hosts out of rotation, override per request), batch waves that halt on failures, audit log.

## Public repository notes

This repo is public. Keep it free of real project, cluster, host and IP names, tokens and keys: real deployment values go in `deploy/k8s/overlays/local/` and `CLAUDE.local.md`, both git-ignored. The dashboard has no authentication, so never expose a real deployment without restricting access.

## Security model

- Agent transport: WebSocket over **mTLS** on `:8443`. Server runs its own CA. First contact uses a one-time enrollment token plus SHA-256 CA pin; each host then gets a client cert (CN = host ID, 90 days, auto-renewed).
- Nomad ACL token and CA key are AES-GCM encrypted in Postgres with `MASTER_KEY`.
- **The dashboard has no authentication yet** (per the current decision). The Ingress is public, so restrict it (IAP, or an allowlist via Cloud Armor / `loadBalancerSourceRanges`) before real use. Anyone who can open it can drain nodes.

## Layout

```
cmd/server, cmd/agent        entrypoints
internal/server              API, hub, commands, batches, metrics, CA, web UI (embedded)
internal/agent               collectors, tart/nomad runners, enrollment
internal/wire                agent <-> server messages
deploy/k8s/base              Kustomize base (namespace, server, postgres, ingress, backup)
deploy/k8s/overlays/example  cluster-specific patches
Dockerfile, Makefile
```

## Server configuration

| Env | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | required | Postgres DSN |
| `MASTER_KEY` | required | 32 bytes, base64 or hex; encrypts secrets |
| `AGENT_HOSTS` | required | Comma list of DNS/IPs agents connect to (goes in the server cert) |
| `PUBLIC_AGENT_URL` | `https://<first AGENT_HOSTS>:8443` | Shown in enrollment commands |
| `HTTP_ADDR` / `AGENT_ADDR` | `:8080` / `:8443` | Listeners |
| `RAW_RETENTION_DAYS` / `RETENTION_DAYS` | 2 / 30 | Raw samples / 5-min rollups |
| `AGENT_CERT_DAYS` | 90 | Client cert lifetime |
| `AGENT_RELEASE_REPO` | `raghavharness/mac-fleet` | GitHub repo that hosts agent releases |
| `AGENT_RELEASE_PRIVATE` | `false` | Set `true` for a private repo; the dashboard then shows token-based download steps |

## Deploy to GKE

One-time:

```sh
gcloud container clusters get-credentials CLUSTER --zone ZONE --project PROJECT_ID

# registry
gcloud artifacts repositories create macfleet --repository-format=docker --location=REGION --project PROJECT_ID

# static IP for the agent endpoint (and optionally one for the dashboard)
gcloud compute addresses create macfleet-agent --region REGION --project PROJECT_ID
gcloud compute addresses describe macfleet-agent --region REGION --project PROJECT_ID --format='value(address)'
```

Copy `deploy/k8s/overlays/example` to `deploy/k8s/overlays/local` (git-ignored) and put that IP in its `kustomization.yaml` (`AGENT_HOSTS`, `PUBLIC_AGENT_URL`, `loadBalancerIP`) and set the dashboard hostname (ManagedCertificate + Ingress host). Then:

```sh
kubectl create namespace macfleet
kubectl -n macfleet create secret generic macfleet-secrets \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 24)" \
  --from-literal=MASTER_KEY="$(openssl rand -base64 32)"
# Keep MASTER_KEY somewhere safe; losing it makes stored secrets unreadable.

make push TAG=v0.1.0
make deploy TAG=v0.1.0
```

If the cluster has no Ingress controller or you don't want a public hostname yet: `kubectl -n macfleet port-forward svc/macfleet-web 8080:80`.

Postgres runs as a StatefulSet (20Gi PVC) with a nightly `pg_dump` CronJob (metrics tables excluded, 7 kept). To use Cloud SQL instead, drop `postgres.yaml` and point `DATABASE_URL` at it.

## Releases and installing the agent

Agent binaries are published as GitHub Releases of this repo, which is public, so Macs download them without any token.

Publish a build manually: `make release TAG=v0.1.0` (needs Go and a logged-in `gh`). It builds both binaries, `SHA256SUMS` and `install.sh` and creates the release. Or, if `.github/workflows/release.yml` is in the repo, `git tag v0.1.0 && git push origin v0.1.0` does the same: the workflow builds `macfleet-agent-darwin-arm64` and `-amd64`, `install.sh` and `SHA256SUMS`, and attaches them to the release.

Install on a Mac, from the dashboard: open **Settings → Install the agent on a Mac**. It shows every step with copy buttons:
1. Click *Create token and command* to get the one-time install command.
2. Run it as the non-root user that runs Nomad and Tart. `scripts/install.sh` downloads the release, verifies its SHA-256, installs it to `~/.macfleet/bin`, enrolls the host and starts a LaunchAgent (`io.harness.macfleet-agent`).
3. The page shows "Connected" when the host appears.

Upgrade an enrolled Mac: rerun the installer with `--upgrade` (the dashboard shows the exact command under *Downloads, upgrade and manual install*).

Set `AGENT_RELEASE_REPO` (default `raghavharness/mac-fleet`) and `AGENT_RELEASE_PRIVATE` (default `false`; set `true` for a private fork, which adds a GitHub-token step) on the server to change where the dashboard points.

Manual install without the script: download the binary, `chmod +x`, then `macfleet-agent enroll --server <url> --ca-sha256 <pin> --token <token>` and `macfleet-agent install-launchd`.

Then, in Settings, set the Nomad address and ACL token and use **Test**.

## Local development

```sh
export DATABASE_URL=postgres://... MASTER_KEY=$(openssl rand -base64 32) AGENT_HOSTS=localhost
go run ./cmd/server
```

## Known limitations

- Transport is WebSocket, not gRPC (the design doc originally said gRPC).
- No dashboard auth; single server replica.
- Nomad config history is `.bak` on the host only (no versioned table yet).
- Verified end to end against a fake Nomad/Tart; not yet against a real `tart list --format json` and real launchd labels. Drain/eligibility/connection tests run on the Macs, so the GKE server does not need to reach the Nomad API (e.g. behind a VPN); at least one Mac in the cluster must be online.
- Kubernetes manifests have not been applied to a cluster.

## Upgrading agents from the dashboard

Agents running v0.1.6 or later can upgrade themselves. The host page shows "Upgrade to vX" when a newer release exists, and the Fleet bulk bar has "Upgrade agent" (rolled out in waves). The server reads the GitHub release and its checksums; the agent verifies the checksum, restarts through launchd and rolls back if the new version cannot reconnect. Older agents need one manual `install.sh --upgrade` first. Requires a public release repository.
