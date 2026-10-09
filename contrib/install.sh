#!/usr/bin/env bash
# Install cooldown as a systemd service that starts at boot.
# Run as root from the repository: sudo bash contrib/install.sh
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
bin="$here/dist/cooldown-linux-amd64"
[[ $EUID -eq 0 ]] || { echo "run as root: sudo bash $0" >&2; exit 1; }
[[ -x $bin ]] || { echo "missing $bin (download it from Releases or run: make linux)" >&2; exit 1; }

# 1. Make sure manual fan control stays enabled after reboots.
echo 'options thinkpad_acpi fan_control=1' > /etc/modprobe.d/cooldown.conf
echo thinkpad_acpi > /etc/modules-load.d/cooldown.conf
echo "==> fan_control=1 set in /etc/modprobe.d/cooldown.conf"

# 2. Install and start the service.
install -m 0755 "$bin" /usr/local/bin/cooldown
install -m 0644 "$here/contrib/cooldown.service" /etc/systemd/system/cooldown.service
systemctl daemon-reload
systemctl enable cooldown.service
systemctl restart cooldown.service
echo "==> cooldown installed and enabled at boot"
echo "    status:    systemctl status cooldown"
echo "    live log:  journalctl -u cooldown -f"
echo "    uninstall: systemctl disable --now cooldown && rm /usr/local/bin/cooldown /etc/systemd/system/cooldown.service"
