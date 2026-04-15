#!/bin/bash
# Source this file to add project functions to your shell:
#   . envsetup.sh

TARGET_HOST="mtg.bec-systems.com"
TARGET_DIR="/opt/jaas-app"
SERVICE="jaas-app"

# Build, deploy, and restart jaas-app on the target server
jaas_deploy() {
	echo "Building jaas-app for linux/amd64..."
	GOOS=linux GOARCH=amd64 go build -o jaas-app \
		-ldflags "-X main.version=$(git describe --tags --always --dirty)" . || return 1

	echo "Deploying to ${TARGET_HOST}..."

	# Copy binary and config files
	scp jaas-app "${TARGET_HOST}:${TARGET_DIR}/jaas-app.new" || return 1
	scp deploy/jaas-app.service "${TARGET_HOST}:${TARGET_DIR}/" || return 1
	scp deploy/env.example "${TARGET_HOST}:${TARGET_DIR}/" || return 1

	# Install and restart on remote
	ssh "${TARGET_HOST}" bash -s <<'REMOTE' || return 1
set -euo pipefail
cd /opt/jaas-app

# Install systemd unit if changed
sudo cp jaas-app.service /etc/systemd/system/jaas-app.service
sudo systemctl daemon-reload
sudo systemctl enable jaas-app

# Swap binary
sudo systemctl stop jaas-app || true
mv jaas-app.new jaas-app
chmod 755 jaas-app
sudo systemctl start jaas-app

echo "Deployed successfully"
sudo systemctl status jaas-app --no-pager
REMOTE

	# Clean up local build artifact
	rm -f jaas-app

	echo "Done."
}
