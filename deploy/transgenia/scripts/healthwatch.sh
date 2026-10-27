#!/bin/sh
# Periodic check (tgn-meetings-healthwatch.timer). Exits non-zero — which
# triggers the e-mail alert through OnFailure — when the container is not
# healthy or when minutes have been stuck in the queue for more than 2 hours.
set -eu
health=$(docker inspect -f '{{.State.Health.Status}}' tgn-meetings 2>/dev/null || echo missing)
if [ "$health" != healthy ]; then
	echo "tgn-meetings health: $health" >&2
	exit 1
fi
# Count only; no room or session data leaves the container.
stuck=$(docker exec tgn-meetings /app/jaas-app verify-db /data/jaas.db 2>/dev/null | sed -n 's/^minutes_stuck=//p')
if [ -n "$stuck" ] && [ "$stuck" -gt 0 ]; then
	echo "minutes stuck > 2h: $stuck" >&2
	exit 1
fi
echo "ok"
