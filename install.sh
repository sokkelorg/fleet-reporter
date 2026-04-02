#!/usr/bin/env bash
set -euo pipefail

REPO="https://raw.githubusercontent.com/sokkelorg/fleet-reporter/main"
DIR="/opt/fleet-reporter"

# Use existing .env if present (allows re-running to update)
if [ -z "${DOMAIN:-}" ] && [ -f "$DIR/.env" ]; then
  source "$DIR/.env"
fi

DOMAIN="${DOMAIN:?Set DOMAIN to your public hostname, e.g. DOMAIN=surtr.sokkel.dev bash install.sh}"

echo "==> Setting up fleet-reporter in ${DIR}"
mkdir -p "$DIR"

echo "==> Downloading compose files"
curl -fsSL "$REPO/compose.yml" -o "$DIR/compose.yml"
curl -fsSL "$REPO/Caddyfile" -o "$DIR/Caddyfile"

echo "==> Writing .env"
cat > "$DIR/.env" <<EOF
DOMAIN=${DOMAIN}
EOF

echo "==> Starting services"
docker compose -f "$DIR/compose.yml" --project-directory "$DIR" up -d --pull always

echo "==> Done."
echo "    Public:  https://${DOMAIN}/status"
echo "    Local:   curl -X POST http://127.0.0.1:4850/metrics -d @payload.json"
