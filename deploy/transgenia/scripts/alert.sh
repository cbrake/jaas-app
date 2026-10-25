#!/bin/sh
# OnFailure handler (tgn-meetings-alert@<unit>.service): e-mails the technical
# contact that a tgn-meetings unit failed. The mail carries the unit name and
# its state only — never logs, room names or meeting data. At most one mail
# per unit every $ALERT_EVERY seconds, so a persistent failure of the 15-min
# health check does not flood the inbox.
set -eu
unit=${1:?unit}
to=${ALERT_TO:-admin@transgenia.org}
every=${ALERT_EVERY:-21600}
state_dir=${STATE_DIRECTORY:-/var/lib/tgn-meetings-alert}
stamp="$state_dir/$(printf '%s' "$unit" | tr -c 'A-Za-z0-9_.-' '_')"

now=$(date +%s)
if [ -f "$stamp" ] && [ $((now - $(cat "$stamp"))) -lt "$every" ]; then
	echo "alert for $unit suppressed (sent less than ${every}s ago)"
	exit 0
fi

state=$(systemctl show -p ActiveState -p Result -p ExecMainStatus "$unit" | tr '\n' ' ')
printf 'La unidad %s falló en %s a las %s.\n\nEstado: %s\n\nRevisar en el servidor:\n  journalctl -u %s -n 50\n  docker ps --filter name=tgn-meetings\n\n(Siguiente aviso de esta unidad como mínimo en %s horas.)\n' \
	"$unit" "$(hostname)" "$(date -u +%Y-%m-%dT%H:%MZ)" "$state" "$unit" "$((every / 3600))" |
	/opt/transgenia-meetings/bin/notify.sh "$to" "[tgn-meetings] Falla: $unit"
echo "$now" >"$stamp"
