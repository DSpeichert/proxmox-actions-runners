package installer

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// sameRelease makes ti an update of its node to the release it is already at, run as the installed parcon, with no
// new image.
func sameRelease(t *testing.T, ti *testInstaller) release.Version {
	t.Helper()
	ti.Self, ti.Image, ti.Yes = ti.BinaryPath, "", true
	return ti.Version
}

func TestUpgradeForceRunsEveryStep(t *testing.T) {
	ti := installed(t)
	v := sameRelease(t, ti)
	ctx := context.Background()
	templates := ti.node.vmsTagged(vmtags.Template)

	// Without --force, the node is at the release.
	must(t, ti.Upgrade(ctx, v, true, false))
	if !strings.Contains(ti.stdout.String(), "nothing needs updating") {
		t.Fatalf("without --force:\n%s", ti.stdout)
	}

	ti.stdout.Reset()
	must(t, ti.Upgrade(ctx, v, true, true))
	out := ti.stdout.String()
	for _, want := range []string{"check the worker network and firewall, and set them up again",
		"render the controller's config", "check the LAN NIC's offloads", "check KVM async page faults",
		"record release 0.2.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("forced update lacks %q:\n%s", want, out)
		}
	}
	n := ti.node
	// Without an image, --force keeps the template.
	if now := n.vmsTagged(vmtags.Template); !slices.Equal(now, templates) {
		t.Errorf("templates %v, then %v", templates, now)
	}
	if !n.ran("systemctl try-restart parcon.service") {
		t.Errorf("the controller wasn't restarted: %v", n.lines())
	}

	// With the same image again, it imports it again; the old template stays for the controller to remove once unused.
	ti.Image = writeImage(t, filepath.Join(ti.dir, "again"), v.String(), "2.338.0")
	must(t, ti.Upgrade(ctx, v, true, true))
	if now := n.vmsTagged(vmtags.Template); len(now) != len(templates)+1 || !slices.Contains(now, templates[0]) {
		t.Errorf("templates %v, then %v", templates, now)
	}
	if !strings.Contains(ti.stdout.String(), "importing a new one anyway") {
		t.Errorf("output:\n%s", ti.stdout)
	}
}

// An update corrects a firewall that drifted, without --force, and keeps workers restricted while it does: new
// group rules go in above the old ones before those go.
func TestUpgradeCorrectsTheFirewall(t *testing.T) {
	ti := installed(t)
	v := sameRelease(t, ti)
	n := ti.node
	n.ipsets[BlockedIPSet] = slices.DeleteFunc(n.ipsets[BlockedIPSet], func(c string) bool { return c == "192.0.2.5" })
	n.ipsets[BlockedIPSet] = append(n.ipsets[BlockedIPSet], "203.0.113.0/24")
	n.hostRules = nil
	n.dns = []string{"192.0.2.53"}

	must(t, ti.Upgrade(context.Background(), v, true, false))
	out := ti.stdout.String()
	for _, want := range []string{"correct the worker network and firewall", "DNS server is 192.0.2.1, not the " +
		"host's 192.0.2.53", "IP set par-blocked lacks 192.0.2.5", "security group par-worker has other rules",
		"DHCP rule for parnet is missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if set := n.ipsets[BlockedIPSet]; !slices.Contains(set, "192.0.2.5") || slices.Contains(set, "203.0.113.0/24") {
		t.Errorf("IP set = %v", set)
	}
	if !sameRules(n.groups[FirewallGroup], workerRules(netipMust("192.0.2.53"))) {
		t.Errorf("group rules = %+v", n.groups[FirewallGroup])
	}
	if len(n.hostRules) != 1 || n.subnets[0].DNSServer != "192.0.2.53" {
		t.Errorf("host rules %+v, subnet %+v", n.hostRules, n.subnets[0])
	}
	// The group never lost its DROP rule: the new rules went in before the old ones were deleted.
	lines := n.lines()
	firstDelete := slices.IndexFunc(lines, func(l string) bool {
		return strings.HasPrefix(l, "pvesh delete /cluster/firewall/groups/")
	})
	lastCreate := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "pvesh create /cluster/firewall/groups/par-worker ") {
			lastCreate = i
		}
	}
	if firstDelete < 0 || lastCreate < 0 || firstDelete < lastCreate {
		t.Errorf("group rules deleted before the new ones were in:\n%s", strings.Join(lines, "\n"))
	}
}

func TestUpgradeRefusesAGatewayInstall(t *testing.T) {
	ti := installed(t)
	v := sameRelease(t, ti)
	ti.node.vms[99] = &fakeVM{id: 99, pool: SystemPool, tags: []string{vmtags.Managed, vmtags.Gateway},
		running: true, config: map[string]string{}}
	err := ti.Upgrade(context.Background(), v, true, false)
	if err == nil || !strings.Contains(err.Error(), "run parcon uninstall with the release that installed it") {
		t.Errorf("Upgrade = %v", err)
	}
}

func TestUpgradeRefusesWithoutTheDatacenterFirewall(t *testing.T) {
	ti := installed(t)
	v := sameRelease(t, ti)
	ti.node.clusterFW = false
	if err := ti.Upgrade(context.Background(), v, true, false); !errors.Is(err, errNoClusterFirewall) {
		t.Errorf("Upgrade = %v", err)
	}
}
