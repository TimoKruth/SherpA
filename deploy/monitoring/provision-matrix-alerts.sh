#!/bin/sh
# Provision a dedicated Matrix bot + alert room for Uptime Kuma.
# Runs on the VPS as root. Secrets stay in a 0600 file on the host.
set -eu

BOT=sherpa-alerts
DOMAIN=matrix.kruth-support.de
OWNER="@krutim:${DOMAIN}"
HS="https://${DOMAIN}"
CREDS=/root/sherpa-alerts-matrix.env

# Refuse to run twice: a second run would orphan the first token and room.
if [ -f "$CREDS" ]; then
	echo "refusing to re-provision: ${CREDS} already exists" >&2
	exit 1
fi

# Alphanumeric only, so it needs no escaping in JSON or shell.
PASS=$(head -c 48 /dev/urandom | base64 | tr -cd 'A-Za-z0-9' | head -c 32)
[ ${#PASS} -eq 32 ] || { echo "password generation failed" >&2; exit 1; }

echo "registering @${BOT}:${DOMAIN} ..."
docker exec matrix_server-synapse-1 register_new_matrix_user \
	-u "$BOT" -p "$PASS" --no-admin \
	-c /data/homeserver.yaml http://localhost:8008 >/dev/null

echo "logging in ..."
TOKEN=$(curl -fsS -X POST "${HS}/_matrix/client/v3/login" \
	-H 'Content-Type: application/json' \
	-d "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"${BOT}\"},\"password\":\"${PASS}\",\"initial_device_display_name\":\"uptime-kuma\"}" \
	| python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')
[ -n "$TOKEN" ] || { echo "login returned no token" >&2; exit 1; }

# The room is deliberately UNENCRYPTED: Uptime Kuma's Matrix provider PUTs a
# plaintext m.room.message and has no E2EE support, so an encrypted room would
# show every alert as an undecryptable message.
echo "creating room ..."
ROOM=$(curl -fsS -X POST "${HS}/_matrix/client/v3/createRoom" \
	-H "Authorization: Bearer ${TOKEN}" \
	-H 'Content-Type: application/json' \
	-d "{
	      \"name\": \"SherpA Alerts\",
	      \"topic\": \"Uptime Kuma alerts for the SherpA beta. Do not enable encryption: the sender cannot encrypt.\",
	      \"preset\": \"private_chat\",
	      \"invite\": [\"${OWNER}\"],
	      \"power_level_content_override\": {\"users\": {\"@${BOT}:${DOMAIN}\": 100, \"${OWNER}\": 100}}
	    }" \
	| python3 -c 'import sys,json; print(json.load(sys.stdin)["room_id"])')
[ -n "$ROOM" ] || { echo "createRoom returned no room_id" >&2; exit 1; }

umask 077
cat > "$CREDS" <<EOF
# Uptime Kuma -> Matrix alert channel. Created $(date -u +%Y-%m-%dT%H:%M:%SZ).
# Revoke with: curl -X POST ${HS}/_matrix/client/v3/logout -H "Authorization: Bearer \$MATRIX_TOKEN"
MATRIX_MXID=@${BOT}:${DOMAIN}
MATRIX_PASSWORD=${PASS}
MATRIX_TOKEN=${TOKEN}
MATRIX_ROOM=${ROOM}
MATRIX_HOMESERVER=${HS}
EOF
chmod 600 "$CREDS"

# Only non-secret identifiers go to stdout.
echo "OK"
echo "MXID=@${BOT}:${DOMAIN}"
echo "ROOM=${ROOM}"
echo "CREDS=${CREDS}"
