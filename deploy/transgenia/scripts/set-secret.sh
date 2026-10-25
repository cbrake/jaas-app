#!/bin/sh
# Store one secret for tgn-meetings without it touching argv, shell history,
# logs or the terminal echo.
#
#   sudo /opt/transgenia-meetings/bin/set-secret.sh openai_api_key
#       (paste the value, Enter)
#   sudo /opt/transgenia-meetings/bin/set-secret.sh jaas_private_key < key.pk
#       (multi-line PEM from a file; then shred the source file)
#
# Files end up root:tgn-meetings 0440, readable by the container user only.
set -eu
umask 077

BASE=/opt/transgenia-meetings
GROUP=tgn-meetings
name=${1:-}
case "$name" in
jaas_private_key | jaas_webhook_secret | openai_api_key | admin_initial_password | smtp_password) ;;
*)
	echo "usage: $0 {jaas_private_key|jaas_webhook_secret|openai_api_key|admin_initial_password|smtp_password} [< file]" >&2
	exit 2
	;;
esac

dest="$BASE/secrets/$name"
tmp=$(mktemp "$BASE/secrets/.${name}.XXXXXX")
trap 'rm -f "$tmp"' EXIT

if [ -t 0 ]; then
	if [ "$name" = jaas_private_key ]; then
		echo "Pega la clave PEM completa y termina con Ctrl-D:" >&2
		cat >"$tmp"
	else
		printf 'Valor para %s (no se mostrará): ' "$name" >&2
		stty -echo
		IFS= read -r value || true
		stty echo
		echo >&2
		printf '%s\n' "$value" >"$tmp"
		value=
	fi
else
	cat >"$tmp"
fi

if [ ! -s "$tmp" ]; then
	echo "vacío: no se guardó nada" >&2
	exit 1
fi
if [ "$name" = jaas_private_key ] && ! grep -q -- '-----BEGIN' "$tmp"; then
	echo "no parece una clave PEM (falta -----BEGIN)" >&2
	exit 1
fi

chown root:"$GROUP" "$tmp"
chmod 0440 "$tmp"
mv -f "$tmp" "$dest"
trap - EXIT
echo "guardado: $dest ($(wc -c <"$dest") bytes, root:$GROUP 0440)" >&2
echo "aplica con: cd $BASE && docker compose up -d --force-recreate" >&2
