# AdGuard External-DNS Sidecar

A lightweight Go sidecar application designed to run alongside [external-dns](https://github.com/kubernetes-sigs/external-dns) when using the AdGuard Home provider. This sidecar ensures that a specific DNS rule always remains at the bottom of the custom filtering rules list, maintaining correct priority.

## Problem Statement

When using external-dns with AdGuard Home, the synchronization process can reorder custom filtering rules. This is problematic when you have a wildcard rule (e.g., `||*.domain.tld^`) that must remain at the bottom of the list to ensure proper DNS resolution priority.

## Solution

This sidecar application polls the AdGuard Home API at regular intervals to:

1. Fetch the current custom filtering rules (user rules)
2. Check if the target rule is at the bottom of the list
3. If not, move it to the bottom position
4. Update AdGuard Home with the corrected rule order

### Why Polling?

AdGuard Home does not provide webhooks or event notifications for configuration changes. Polling is the only reliable method to detect when external-dns has modified the rules and ensure the target rule maintains its position.

## Features

- **Lightweight**: Built as a statically-linked binary using Go, running in a `scratch` Docker image (~5-10MB)
- **Simple Configuration**: All settings via environment variables
- **Secure**: Supports HTTPS connections to AdGuard Home with bundled CA certificates
- **Resilient**: Continues running even if AdGuard Home is temporarily unavailable
- **Non-Destructive**: Only modifies rule order, never deletes or modifies rule content

## Configuration

All configuration is done via environment variables:

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `ADGUARD_URL` | ✅ Yes | - | AdGuard Home URL (e.g., `http://192.168.1.1:3000` or `https://adguard.example.com`) |
| `ADGUARD_USER` | ✅ Yes | - | AdGuard Home admin username |
| `ADGUARD_PASS` | ✅ Yes | - | AdGuard Home admin password |
| `TARGET_RULE` | ✅ Yes | - | The exact rule that must stay at the bottom (e.g., `\|\|*.domain.tld^$dnsrewrite=NOERROR;A;192.168.1.100`) |
| `CHECK_INTERVAL` | ❌ No | `60` | How often to check (in seconds) |
| `HEALTH_PORT` | ❌ No | `8080` | Port for health check endpoints (`/healthz` and `/readyz`) |

### Important Notes

- **ADGUARD_URL**: Do not include a trailing slash
- **TARGET_RULE**: Must match the rule exactly as it appears in AdGuard Home
- **CHECK_INTERVAL**: Recommended range is 30-300 seconds. Lower values increase API load.

## Quick Start

### Using Docker Compose

1. Clone this repository:
   ```bash
   git clone https://github.com/will-white/adguard-external-dns-sidecar.git
   cd adguard-external-dns-sidecar
   ```

2. Edit `docker-compose.yml` with your configuration:
   ```yaml
   environment:
     ADGUARD_URL: "http://192.168.1.1:3000"
     ADGUARD_USER: "admin"
     ADGUARD_PASS: "your-password"
     TARGET_RULE: "||*.domain.tld^$dnsrewrite=NOERROR;A;192.168.1.100"
     CHECK_INTERVAL: "60"
   ```

3. Build and run:
   ```bash
   docker-compose up -d
   ```

4. Check logs:
   ```bash
   docker-compose logs -f adguard-sidecar
   ```

### Using Docker

```bash
# Build the image
docker build -t adguard-sidecar .

# Run the container
docker run -d \
  --name adguard-sidecar \
  --restart unless-stopped \
  -e ADGUARD_URL="http://192.168.1.1:3000" \
  -e ADGUARD_USER="admin" \
  -e ADGUARD_PASS="your-password" \
  -e TARGET_RULE="||*.domain.tld^$dnsrewrite=NOERROR;A;192.168.1.100" \
  -e CHECK_INTERVAL="60" \
  adguard-sidecar
```

### Kubernetes Deployment

When deploying alongside external-dns in Kubernetes, you can add this as a sidecar container:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: external-dns
spec:
  template:
    spec:
      containers:
      - name: external-dns
        image: registry.k8s.io/external-dns/external-dns:v0.14.0
        # ... external-dns configuration ...
      
      # Add the sidecar
      - name: adguard-sidecar
        image: adguard-sidecar:latest
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
        - name: TARGET_RULE
          value: "||*.domain.tld^$dnsrewrite=NOERROR;A;192.168.1.100"
        - name: CHECK_INTERVAL
          value: "60"
```

## Building from Source

```bash
# Clone the repository
git clone https://github.com/will-white/adguard-external-dns-sidecar.git
cd adguard-external-dns-sidecar

# Build locally
go build -o adguard-sidecar main.go

# Run
export ADGUARD_URL="http://192.168.1.1:3000"
export ADGUARD_USER="admin"
export ADGUARD_PASS="your-password"
export TARGET_RULE="||*.domain.tld^$dnsrewrite=NOERROR;A;192.168.1.100"
export CHECK_INTERVAL="60"
./adguard-sidecar
```

## How It Works

1. **Startup**: The application validates all required environment variables
2. **Initial Check**: Runs immediately on startup to ensure the rule is in the correct position
3. **Polling Loop**: Every `CHECK_INTERVAL` seconds:
   - Fetches current rules from AdGuard Home via `/control/filtering/status`
   - Checks if `TARGET_RULE` is at the bottom
   - If not (or missing), removes it from its current position and appends to the end
   - Updates AdGuard Home via `/control/filtering/set_rules`
4. **Logging**: All actions are logged for visibility and debugging

## Logging

The application provides clear logging output:

```
2025/11/25 10:00:00 Starting AdGuard External-DNS Sidecar...
2025/11/25 10:00:00 Configuration loaded: URL=http://192.168.1.1:3000, Target Rule=||*.domain.tld^, Check Interval=1m0s
2025/11/25 10:00:00 Fetched 15 user rules from AdGuard
2025/11/25 10:00:00 Moving target rule to bottom position (rule 15 of 15)
2025/11/25 10:00:00 Successfully updated user rules in AdGuard
```

## Troubleshooting

### Application exits immediately
- Check that all required environment variables are set
- Verify credentials are correct

### "Connection refused" errors
- Ensure `ADGUARD_URL` is correct and accessible
- Check network connectivity between the sidecar and AdGuard Home

### HTTPS certificate errors
- The application includes CA certificates for HTTPS
- Ensure the AdGuard Home certificate is valid

### Rule not being maintained at bottom
- Verify `TARGET_RULE` matches exactly (including spacing and special characters)
- Check application logs for errors
- Increase logging verbosity if needed

## License

MIT

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.

## Author

Built for use with external-dns and AdGuard Home.
