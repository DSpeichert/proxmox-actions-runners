#!/usr/bin/env bash
# Automatic security updates for the controller image, after base.sh. The controller VM is long-lived, unlike
# workers, so it keeps itself patched with unattended-upgrades.
set -euo pipefail

main() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get -y install --no-install-recommends unattended-upgrades
  systemctl enable unattended-upgrades
  cat >/etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
}

main "$@"
