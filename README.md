# AdGuard External-DNS Sidecar

A lightweight Go sidecar that runs alongside [external-dns](https://github.com/kubernetes-sigs/external-dns) with the AdGuard Home provider. It maintains a single **catch-all DNS rewrite** for your root domain in AdGuard Home's custom filtering rules, automatically exempting every hostname that actually exists as a Kubernetes Ingress.

## Problem Statement

With external-dns + AdGuard Home you typically want two things at once:

1. **Real services resolve to their real target** — external-dns creates a rewrite per Ingress host.
2. **Everything else under the domain still resolves somewhere** — e.g. `*.domain.tld` → your reverse proxy or a landing page, so unknown or not-yet-created hostnames do not simply fail.

A naive wildcard rewrite (`||*.domain.tld^$dnsrewrite=...`) breaks (1): it swallows hostnames that external-dns manages. Rule ordering does not reliably fix this, and external-dns rewrites the rule list on every sync anyway.

## Solution

The sidecar polls the Kubernetes API and AdGuard Home, and keeps one **managed block** at the end of the user rules list:

```
! -- ADGUARD EXTERNAL DNS SIDECAR START --
||*.domain.tld^$dnsrewrite=NOERROR;A;192.168.1.100,denyallow=hass.domain.tld|grafana.domain.tld
! -- ADGUARD EXTERNAL DNS SIDECAR END --
```

The `$denyallow` modifier lists every Ingress host under your root domain, exempting them from the catch-all so the specific external-dns rewrites (or upstream DNS) take effect. Any other name under the domain falls through to `FALLBACK_IP`.

The block is delimited by sentinel comments, so the sidecar can rewrite its own rule on every sync without touching hand-written rules.

### Why Polling?

AdGuard Home provides no webhooks or event notifications for configuration changes. Polling is the only reliable way to detect that external-dns has changed the rules or that an Ingress was added or removed.

## Features

- **Lightweight**: statically linked binary in a `scratch` image (~11 MB), multi-arch (`linux/amd64`, `linux/arm64`)
- **Zero dependencies**: Go standard library only — no third-party modules
- **Self-configuring**: exemptions are derived from live Ingress resources, not a hand-maintained list
- **Idempotent**: AdGuard is only written to when the computed rules actually differ
- **Non-destructive**: rules outside the managed block are preserved
- **Secure**: runs as UID 65534, HTTPS with bundled CA certificates

## Configuration

All configuration is via environment variables:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `ADGUARD_URL` | ✅ Yes | - | AdGuard Home URL, no trailing slash (e.g. `http://192.168.1.1:3000`) |
| `ADGUARD_USER` | ✅ Yes | - | AdGuard Home admin username |
| `ADGUARD_PASS` | ✅ Yes | - | AdGuard Home admin password |
| `ROOT_DOMAIN` | ✅ Yes | - | Root domain to manage, e.g. `domain.tld` (a leading dot is stripped) |
| `FALLBACK_IP` | ✅ Yes | - | IP that unmatched hosts under `ROOT_DOMAIN` rewrite to |
| `CHECK_INTERVAL` | ❌ No | `60` | Seconds between syncs |
| `HEALTH_PORT` | ❌ No | `8080` | Port for the health endpoints |

### Important Notes

- Only Ingress hosts ending in `.ROOT_DOMAIN` become `denyallow` entries; hosts on other domains are ignored.
- The wildcard `||*.domain.tld^` matches subdomains, **not** the apex `domain.tld` itself.
- `CHECK_INTERVAL`: 30–300 seconds is a sensible range. Lower values increase load on both APIs.
- Missing or invalid required variables cause an immediate exit — the sidecar fails fast on misconfiguration but retries indefinitely on API errors.

## Health Endpoints

| Endpoint | Behaviour |
|----------|-----------|
| `/readyz` | Always `200 READY` once the process is up |
| `/healthz` | `200 OK` only while the last sync succeeded; `503` otherwise |

`/healthz` deliberately reports failure when AdGuard is unreachable, so **do not use it as a Kubernetes liveness probe** — a brief AdGuard outage would restart the sidecar in a loop for no benefit. Use `/readyz` for liveness and `/healthz` for readiness (or scrape it for alerting).

The container's Docker `HEALTHCHECK` calls the binary's own `-health` flag, since the `scratch` image contains no shell or `curl`.

## Quick Start

Images are published to GHCR on release:

```
ghcr.io/will-white/adguard-external-dns-sidecar:latest
ghcr.io/will-white/adguard-external-dns-sidecar:v0.3.14
```

### Using Docker Compose

1. Edit `docker-compose.yml` with your configuration:
   ```yaml
   environment:
     ADGUARD_URL: "http://192.168.1.1:3000"
     ADGUARD_USER: "admin"
     ADGUARD_PASS: "your-password"
     ROOT_DOMAIN: "domain.tld"
     FALLBACK_IP: "192.168.1.100"
     CHECK_INTERVAL: "60"
   ```

2. Build and run:
   ```bash
   docker compose up -d
   docker compose logs -f adguard-sidecar
   ```

Outside Kubernetes there is no Ingress API to query, so the sidecar logs `using MockClient` and generates the wildcard rule with **no** `denyallow` exemptions. Useful for testing connectivity to AdGuard; not useful in production.

### Using Docker

```bash
docker run -d \
  --name adguard-sidecar \
  --restart unless-stopped \
  -e ADGUARD_URL="http://192.168.1.1:3000" \
  -e ADGUARD_USER="admin" \
  -e ADGUARD_PASS="your-password" \
  -e ROOT_DOMAIN="domain.tld" \
  -e FALLBACK_IP="192.168.1.100" \
  ghcr.io/will-white/adguard-external-dns-sidecar:latest
```

### Kubernetes Deployment

The sidecar reads Ingress resources across all namespaces using the pod's service account token, so **the service account needs cluster-wide `list` on `ingresses`**. Without it, every sync fails with a 403.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: external-dns
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: adguard-sidecar-ingress-reader
rules:
- apiGroups: ["networking.k8s.io"]
  resources: ["ingresses"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: adguard-sidecar-ingress-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: adguard-sidecar-ingress-reader
subjects:
- kind: ServiceAccount
  name: external-dns
  namespace: default
```

> external-dns' own ClusterRole normally already grants `list` on ingresses. If the sidecar shares its pod (and therefore its service account), the binding above may be redundant — check before adding it.

Then add the container to the external-dns Deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: external-dns
spec:
  template:
    spec:
      serviceAccountName: external-dns
      containers:
      - name: external-dns
        image: registry.k8s.io/external-dns/external-dns:v0.14.0
        # ... external-dns configuration ...

      # Add the sidecar
      - name: adguard-sidecar
        image: ghcr.io/will-white/adguard-external-dns-sidecar:latest
        env:
        - name: ADGUARD_URL
          value: "http://adguard.default.svc.cluster.local:3000"
        - name: ADGUARD_USER
          valueFrom:
            secretKeyRef:
              name: adguard-credentials
              key: username
        - name: ADGUARD_PASS
          valueFrom:
            secretKeyRef:
              name: adguard-credentials
              key: password
        - name: ROOT_DOMAIN
          value: "domain.tld"
        - name: FALLBACK_IP
          value: "192.168.1.100"
        - name: CHECK_INTERVAL
          value: "60"
        ports:
        - name: health
          containerPort: 8080
        livenessProbe:
          httpGet:
            path: /readyz
            port: health
        readinessProbe:
          httpGet:
            path: /healthz
            port: health
```

## Building from Source

Requires Go as declared in `go.mod` (currently 1.26); `go.mod` is the single source of truth and CI verifies the Dockerfile builder image matches it.

```bash
git clone https://github.com/will-white/adguard-external-dns-sidecar.git
cd adguard-external-dns-sidecar

go build -o adguard-sidecar .
go test -race -cover ./...

export ADGUARD_URL="http://192.168.1.1:3000"
export ADGUARD_USER="admin"
export ADGUARD_PASS="your-password"
export ROOT_DOMAIN="domain.tld"
export FALLBACK_IP="192.168.1.100"
./adguard-sidecar
```

Or build the image (`--platform` optional; the build cross-compiles):

```bash
docker build -t adguard-sidecar .
```

A devcontainer is provided under `.devcontainer/` for a preconfigured Go toolchain.

## How It Works

1. **Startup**: validates required environment variables (exits immediately if any are missing) and starts the health server.
2. **Client selection**: uses the in-cluster API client when `KUBERNETES_SERVICE_HOST` is set, otherwise a mock client returning no hosts.
3. **Every `CHECK_INTERVAL` seconds** (and once immediately on startup):
   - `GET /apis/networking.k8s.io/v1/ingresses` — collect unique Ingress hosts
   - `GET /control/filtering/status` — fetch AdGuard's current user rules
   - Rebuild the list: keep all rules outside the managed block, then append a freshly generated block containing the wildcard rewrite and `denyallow` exemptions
   - `POST /control/filtering/set_rules` — **only if the result differs** from what AdGuard already has
4. **Migration**: on the first run against a pre-sentinel deployment, legacy static-wildcard and regex rewrite rules for the root domain are removed. This cleanup stops as soon as a managed block exists.

## Logging

```
2026/08/11 10:00:00 Starting AdGuard External-DNS Sidecar...
2026/08/11 10:00:00 Configuration loaded: URL=http://192.168.1.1:3000, Root=domain.tld, IP=192.168.1.100, Check Interval=1m0s
2026/08/11 10:00:00 Health server listening on port 8080
2026/08/11 10:00:00 Updating user rules in AdGuard (Total: 16)
2026/08/11 10:00:00 Successfully updated user rules in AdGuard
```

Credentials are never logged. When nothing has changed, a sync logs nothing.

## Troubleshooting

### Application exits immediately
All of `ADGUARD_URL`, `ADGUARD_USER`, `ADGUARD_PASS`, `ROOT_DOMAIN`, `FALLBACK_IP` must be set; the log names the missing one.

### `failed to fetch ingress hosts: API server returned 403`
The pod's service account lacks cluster-wide `list` on `ingresses` — see the RBAC manifests above.

### Rule has no `denyallow` entries
Either no Ingress host ends in `.ROOT_DOMAIN`, or the sidecar is running outside Kubernetes (look for `using MockClient` in the logs).

### "Connection refused" errors
Check `ADGUARD_URL` (no trailing slash) and network reachability from the pod to AdGuard Home.

### Rules keep reappearing or fighting
Confirm only one sidecar instance manages a given `ROOT_DOMAIN`, and that nothing else edits rules between the sentinel comments — that region is regenerated on every sync.

### `/healthz` returns 503
The last sync failed; the reason is in the logs. This is expected while AdGuard is unreachable and clears on the next successful sync.

## License

MIT
