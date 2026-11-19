#!/usr/bin/env bash
# Runs on the test node as root, sent by run.sh. Gives a test template the worker firewall the installer gives the
# runner template: on, DHCP allowed, nothing in, and the suite's security group, which the controller checks for
# before it starts a worker. Clones copy it. Safe to re-run.
#
# Environment: VMID GROUP
set -euo pipefail

main() {
	: "${VMID:?}" "${GROUP:?}"
	local node path
	node=$(hostname)
	path=/nodes/$node/qemu/$VMID/firewall
	echo "== firewall of template $VMID: group $GROUP"
	pvesh set "$path/options" --enable 1 --dhcp 1 --macfilter 1 --policy_in DROP --policy_out ACCEPT
	if ! pvesh get "$path/rules" --output-format json | perl -MJSON -e '
		my $g = $ARGV[0];
		exit !grep { $_->{type} eq "group" && $_->{action} eq $g } @{decode_json(do { local $/; <STDIN> })};' "$GROUP"; then
		pvesh create "$path/rules" --type group --action "$GROUP" --enable 1
	fi
}

main "$@"
