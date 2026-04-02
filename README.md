# fleet-reporter

Lightweight HTTP server that collects metrics from a simulation runner and exposes them via a public status endpoint.

## Install

Requires Docker and Docker Compose on the target server.

```bash
curl -fsSL https://raw.githubusercontent.com/sokkelorg/fleet-reporter/main/install.sh | sudo DOMAIN=status.example.com bash
```

To update, run the same command again. If DOMAIN is already configured it will be read from the existing install.

This sets up two containers:

- **fleet-reporter** — accepts metrics on `127.0.0.1:4850` (local only)
- **caddy** — reverse proxy exposing only `/status` publicly with automatic HTTPS

## Usage

### Report metrics (from the simulation runner)

```bash
curl -X POST http://127.0.0.1:4850/metrics \
  -H "Content-Type: application/json" \
  -d @payload.json
```

### Query status

```bash
# Latest report
curl https://status.example.com/status

# Last N reports
curl https://status.example.com/status?last=10

# All reports since a timestamp
curl https://status.example.com/status?since=2026-04-02T10:00:00Z
```

## Configuration

| Environment variable | Default | Description |
|---|---|---|
| `DOMAIN` | *(required)* | Public hostname for HTTPS |
| `LISTEN_ADDR` | `:4850` | Address the app listens on |
| `DB_PATH` | `fleet-reporter.db` | Path to the SQLite database |

Data is persisted in Docker volumes. The database is automatically cleaned when it exceeds 100 GB.
