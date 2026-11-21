package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/term"
)

func check(t *testing.T, checks []Check, desc string) Check {
	t.Helper()
	for _, c := range checks {
		if strings.HasPrefix(c.Desc, desc) {
			return c
		}
	}
	t.Fatalf("no check %q", desc)
	return Check{}
}

func TestPreflightNetworkAndFirewall(t *testing.T) {
	for _, tt := range []struct {
		name      string
		breakNode func(n *fakeNode)
		check     string
		reason    string
	}{
		{"no dnsmasq", func(n *fakeNode) { n.uninstalled["dnsmasq"] = true }, "dnsmasq for SDN DHCP",
			"apt install dnsmasq"},
		{"dnsmasq's own service runs", func(n *fakeNode) { n.services["dnsmasq.service"] = true },
			"dnsmasq for SDN DHCP", "systemctl disable --now dnsmasq"},
		{"datacenter firewall off", func(n *fakeNode) { n.clusterFW = false }, "datacenter firewall on",
			"Datacenter > Firewall > Options"},
		{"no DNS server", func(n *fakeNode) { n.dns = nil }, "DNS server for workers", "no IPv4 DNS server"},
		{"only a local resolver", func(n *fakeNode) { n.dns = []string{"127.0.0.1", "::1"} }, "DNS server for workers",
			"no IPv4 DNS server"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ti := newTestInstaller(t)
			s := settings.Default()
			if c := check(t, ti.Preflight(context.Background(), &s, ModeInstall), tt.check); !c.OK {
				t.Fatalf("before: %+v", c)
			}
			tt.breakNode(ti.node)
			checks := ti.Preflight(context.Background(), &s, ModeInstall)
			if c := check(t, checks, tt.check); c.OK || c.Level != Hard || !strings.Contains(c.Reason, tt.reason) {
				t.Errorf("%+v, want a hard failure saying %q", c, tt.reason)
			}
		})
	}
	// The second DNS server serves when the first is the node's own.
	ti := newTestInstaller(t)
	ti.node.dns = []string{"127.0.0.53", "192.0.2.53"}
	s := settings.Default()
	if c := check(t, ti.Preflight(context.Background(), &s, ModeInstall), "DNS server"); !c.OK ||
		!strings.Contains(c.Reason, "192.0.2.53") {
		t.Errorf("%+v", c)
	}
}

func TestWorkerNetwork(t *testing.T) {
	s := settings.Default()
	for subnet, want := range map[string][3]string{
		"10.251.0.0/22":   {"10.251.0.1", "10.251.0.2", "10.251.3.254"},
		"192.0.2.0/29":    {"192.0.2.1", "192.0.2.2", "192.0.2.6"},
		"198.51.100.0/24": {"198.51.100.1", "198.51.100.2", "198.51.100.254"},
	} {
		s.Network.WorkerSubnet = subnet
		w, err := workerNetwork(&s)
		if err != nil || [3]string{w.gateway.String(), w.first.String(), w.last.String()} != want {
			t.Errorf("%s: %+v, %v, want %v", subnet, w, err, want)
		}
	}
}

func TestWorkerSubnetMustBeFree(t *testing.T) {
	ti := newTestInstaller(t)
	s := settings.Default()
	s.Network.WorkerSubnet = "192.0.2.0/24"
	c := check(t, ti.Preflight(context.Background(), &s, ModeInstall), "worker subnet")
	if c.OK || !strings.Contains(c.Reason, "overlaps 192.0.2.5/24 on this host") {
		t.Errorf("host network: %+v", c)
	}

	// Our own VNet's address and route, from an earlier run, aren't a clash.
	ti.node.routes = `[{"dst":"10.251.0.0/22","dev":"parnet"}]`
	ti.node.addrs = `[{"ifname":"parnet","addr_info":[{"local":"10.251.0.1","prefixlen":22}]}]`
	s = settings.Default()
	if c := check(t, ti.Preflight(context.Background(), &s, ModeInstall), "worker subnet"); !c.OK {
		t.Errorf("our own route: %+v", c)
	}
}

func TestNamesInUseBySomeoneElse(t *testing.T) {
	ti := newTestInstaller(t)
	ti.node.zones = []string{Zone}
	ti.node.roles[Role] = "VM.Audit"
	ti.node.users = []string{PVEUser}
	ti.node.groups[FirewallGroup] = nil
	s := settings.Default()
	c := check(t, ti.Preflight(context.Background(), &s, ModeInstall), "names free")
	if c.OK || c.Reason != "already exist and weren't created by parcon: firewall security group par-worker, "+
		"SDN zone parzone, role PARController, user par@pve" {
		t.Errorf("%+v", c)
	}
	// An earlier run's objects are ours.
	ti.node.pools[RunnerPool] = PoolComment
	if c := check(t, ti.Preflight(context.Background(), &s, ModeInstall), "names free"); !c.OK {
		t.Errorf("ours: %+v", c)
	}
}

func TestVMIDsMustBeFreeOrOurs(t *testing.T) {
	ti := newTestInstaller(t)
	ti.node.vms[10005] = &fakeVM{id: 10005, pool: "other", config: map[string]string{}}
	ti.node.vms[10006] = &fakeVM{id: 10006, pool: RunnerPool, config: map[string]string{}}
	ti.node.vms[9999] = &fakeVM{id: 9999, config: map[string]string{}}
	s := settings.Default()
	c := check(t, ti.Preflight(context.Background(), &s, ModeInstall), "VMIDs")
	if c.OK || !strings.HasPrefix(c.Reason, "VMIDs 10005 in 10000-10099 belong to other VMs") {
		t.Errorf("%+v", c)
	}
}

func TestInstalledChecksSkipInstallSpace(t *testing.T) {
	ti := newTestInstaller(t)
	ti.node.storage.Avail = 10 << 30
	s := settings.Default()
	for _, c := range ti.Preflight(context.Background(), &s, ModeInstalled) {
		if strings.HasPrefix(c.Desc, "free space") {
			t.Errorf("an installed node checks %s", c.Desc)
		}
	}
}

func TestCheckLines(t *testing.T) {
	for _, tt := range []struct {
		c    Check
		want string
	}{
		{Check{Level: Hard, Desc: "KVM available", OK: true}, "ok    KVM available"},
		{Check{Level: Hard, Desc: "storage x", Reason: "x isn't active"}, "FAIL  storage x: x isn't active"},
		{Check{Level: Warn, Desc: "memory", Reason: "1 MiB RAM"}, "WARN  memory: 1 MiB RAM"},
	} {
		if got := tt.c.String(); got != tt.want {
			t.Errorf("%q, want %q", got, tt.want)
		}
	}
}

// virtioNIC makes nic a virtio NIC on vmbr0 in the fake sysfs, with its offloads on.
func (ti *testInstaller) virtioNIC(t *testing.T, nic string) {
	t.Helper()
	sys := ti.Sys.Paths.SysNet
	must(t, os.WriteFile(filepath.Join(sys, "vmbr0/brif", nic), nil, 0o644))
	drv := filepath.Join(filepath.Dir(sys), "drivers/virtio_net")
	must(t, os.MkdirAll(drv, 0o755))
	must(t, os.MkdirAll(filepath.Join(sys, nic, "device"), 0o755))
	must(t, os.Symlink(drv, filepath.Join(sys, nic, "device/driver")))
	ti.node.offloadsOn[nic] = true
}

func (ti *testInstaller) withEthtool(t *testing.T) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(ti.Sys.Paths.Ethtool), 0o755))
	must(t, os.WriteFile(ti.Sys.Paths.Ethtool, nil, 0o755))
}

func TestOffloadsOnANodeThatIsNotAVM(t *testing.T) {
	ti := newTestInstaller(t)
	ti.withEthtool(t)
	must(t, os.WriteFile(filepath.Join(ti.Sys.Paths.SysNet, "vmbr0/brif/tap100i0"), nil, 0o644))
	s := settings.Default()
	if c := ti.checkOffloads(context.Background(), &s, ModeCheck); !c.ok || c.reason != "not needed: no virtio NIC on vmbr0" {
		t.Errorf("check = %+v", c)
	}
	if plan := ti.offloadPlan(context.Background(), &s); plan != nil {
		t.Errorf("plan = %q", plan)
	}
	must(t, ti.tuneOffloads(context.Background(), &s))
	if _, err := os.Stat(ti.Sys.Paths.UdevRule); !errors.Is(err, os.ErrNotExist) {
		t.Error("wrote a rule")
	}
}

func TestOffloadsOnANodeThatIsAVM(t *testing.T) {
	ti := newTestInstaller(t)
	ti.withEthtool(t)
	ti.virtioNIC(t, "nic0")
	must(t, os.WriteFile(filepath.Join(ti.Sys.Paths.SysNet, "vmbr0/brif/tap100i0"), nil, 0o644))
	ctx := context.Background()
	s := settings.Default()

	if c := ti.checkOffloads(ctx, &s, ModeInstall); !c.ok || c.reason != "the node is a VM: they will be turned off on nic0" {
		t.Errorf("install check = %+v", c)
	}
	c := ti.checkOffloads(ctx, &s, ModeCheck)
	if c.ok || !strings.Contains(c.reason, "udev rule "+ti.Sys.Paths.UdevRule+"; nic0: generic-receive-offload "+
		"generic-segmentation-offload tcp-segmentation-offload tx-checksumming on") {
		t.Errorf("check before = %+v", c)
	}
	if plan := ti.offloadPlan(ctx, &s); len(plan) != 2 || !strings.Contains(plan[0],
		"turn off offloads (gro gso tso tx) on nic0, the virtio NIC under vmbr0") {
		t.Errorf("plan = %q", plan)
	}

	must(t, ti.tuneOffloads(ctx, &s))
	rule, _ := os.ReadFile(ti.Sys.Paths.UdevRule)
	want := `ACTION=="add", SUBSYSTEM=="net", NAME=="nic0", RUN+="` + ti.Sys.Paths.Ethtool +
		` -K nic0 gro off gso off tso off tx off"`
	if !strings.Contains(string(rule), want) {
		t.Errorf("rule = %s", rule)
	}
	if st, _ := os.Stat(ti.Sys.Paths.UdevRule); st.Mode().Perm() != 0o644 {
		t.Errorf("rule mode = %v", st.Mode())
	}
	if !ti.node.ran(ti.Sys.Paths.Ethtool+" -K nic0 gro off gso off tso off tx off") || ti.node.ran("ethtool -K tap100i0") {
		t.Errorf("ethtool calls: %v", ti.node.lines())
	}

	// Done: the check passes, the plan is empty, and another run changes nothing.
	if c := ti.checkOffloads(ctx, &s, ModeCheck); !c.ok || c.reason != "off on nic0" {
		t.Errorf("check after = %+v", c)
	}
	if plan := ti.offloadPlan(ctx, &s); plan != nil {
		t.Errorf("plan after = %q", plan)
	}
	ti.node.calls = nil
	ti.stdout.Reset()
	must(t, ti.tuneOffloads(ctx, &s))
	if strings.Contains(ti.stdout.String(), "$ ") {
		t.Errorf("second run changed:\n%s", ti.stdout)
	}

	// Uninstall turns them back on and removes the rule.
	must(t, ti.removeOffloads(ctx))
	if !ti.node.offloadsOn["nic0"] {
		t.Error("offloads still off")
	}
	if _, err := os.Stat(ti.Sys.Paths.UdevRule); !errors.Is(err, os.ErrNotExist) {
		t.Error("rule left")
	}
}

func TestOffloadsWithoutEthtool(t *testing.T) {
	ti := newTestInstaller(t)
	ti.virtioNIC(t, "nic0")
	s := settings.Default()
	if c := ti.checkOffloads(context.Background(), &s, ModeCheck); c.ok || c.reason != "ethtool isn't installed, so they can't be turned off" {
		t.Errorf("check = %+v", c)
	}
	must(t, ti.tuneOffloads(context.Background(), &s))
	if !strings.Contains(ti.stderr.String(), "ethtool isn't installed") {
		t.Errorf("stderr = %s", ti.stderr)
	}
	if _, err := os.Stat(ti.Sys.Paths.UdevRule); !errors.Is(err, os.ErrNotExist) {
		t.Error("wrote a rule")
	}
}

func TestOffloadsDryRun(t *testing.T) {
	ti := newTestInstaller(t)
	ti.withEthtool(t)
	ti.virtioNIC(t, "nic0")
	ti.Change.DryRun = true
	s := settings.Default()
	must(t, ti.tuneOffloads(context.Background(), &s))
	if !strings.Contains(ti.stdout.String(), "$ "+ti.Sys.Paths.Ethtool+" -K nic0 gro off") {
		t.Errorf("output:\n%s", ti.stdout)
	}
	if _, err := os.Stat(ti.Sys.Paths.UdevRule); !errors.Is(err, os.ErrNotExist) || !ti.node.offloadsOn["nic0"] {
		t.Error("the dry run changed something")
	}
}

func TestInstallationTarget(t *testing.T) {
	org := installation{Account: "my-org", AccountType: "Organization", Repositories: []string{"a"}}
	if got, err := installationTarget(org, "", &term.Scripted{}); err != nil || got != "https://github.com/my-org" {
		t.Errorf("organization = %q, %v", got, err)
	}
	one := installation{Account: "me", AccountType: "User", Repositories: []string{"proj"}}
	if got, err := installationTarget(one, "", &term.Scripted{}); err != nil || got != "https://github.com/me/proj" {
		t.Errorf("one repository = %q, %v", got, err)
	}
	none := installation{Account: "me", AccountType: "User"}
	if _, err := installationTarget(none, "", &term.Scripted{}); err == nil {
		t.Error("no repository: no error")
	}
	several := installation{Account: "me", AccountType: "User", Repositories: []string{"a", "b"}}
	if got, err := installationTarget(several, "https://github.com/me/b", &term.Scripted{}); err != nil ||
		got != "https://github.com/me/b" {
		t.Errorf("several, chosen by flag = %q, %v", got, err)
	}
	// GitHub compares names without regard to case; a different case must not fall through to a guess.
	if got, err := installationTarget(several, "https://github.com/Me/B", &term.Scripted{}); err != nil ||
		got != "https://github.com/me/b" {
		t.Errorf("several, chosen by flag in another case = %q, %v", got, err)
	}
	if _, err := installationTarget(several, "https://github.com/me/c", &term.Scripted{Answers: []string{"1"}}); err == nil ||
		!strings.Contains(err.Error(), "doesn't reach https://github.com/me/c") {
		t.Errorf("several, a repository the installation doesn't reach: %v", err)
	}
	if got, err := installationTarget(several, "", &term.Scripted{Answers: []string{"1"}}); err != nil ||
		got != "https://github.com/me/a" {
		t.Errorf("several, chosen on the terminal = %q, %v", got, err)
	}
	if _, err := installationTarget(several, "", &term.Scripted{NoTTY: true}); err == nil ||
		!strings.Contains(err.Error(), "--github-url https://github.com/me/<repo>") {
		t.Errorf("several without a terminal: %v", err)
	}
}

func TestNewState(t *testing.T) {
	a, b := newState(), newState()
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(a) || a == b {
		t.Errorf("states %q, %q", a, b)
	}
}

func TestVMIDChoices(t *testing.T) {
	r := config.VMIDRange{Start: 10000, End: 10099}
	vms := []proxmox.VM{{VMID: 10096}, {VMID: 10097}}
	if id, err := freeReservedVMID(vms, r); err != nil || id != 10098 {
		t.Errorf("freeReservedVMID = %d, %v", id, err)
	}
	if _, err := freeReservedVMID(append(vms, proxmox.VM{VMID: 10098}, proxmox.VM{VMID: 10099}), r); err == nil {
		t.Error("full reserved IDs: no error")
	}

}

func TestManagedVMsOrder(t *testing.T) {
	vms := []proxmox.VM{
		{VMID: 10099, Pool: RunnerPool, Template: true, Tags: []string{"par-managed", "par-template"}},
		{VMID: 10000, Pool: RunnerPool, Tags: []string{"par-managed", "par-worker"}},
		{VMID: 10001, Pool: RunnerPool},
		{VMID: 101, Pool: SystemPool, Tags: []string{"par-managed", "par-gateway"}},
		{VMID: 200, Pool: "elsewhere", Tags: []string{"par-managed"}},
	}
	if got := vmids(managedVMs(vms)); got != "101 10000 10099" {
		t.Errorf("managedVMs = %s", got)
	}
}

func TestBlocked(t *testing.T) {
	ti := newTestInstaller(t)
	ti.node.addrs = `[{"ifname":"lo","addr_info":[{"local":"127.0.0.1","prefixlen":8}]},
		{"ifname":"vmbr0","addr_info":[{"local":"192.0.2.5","prefixlen":24}]},
		{"ifname":"vmbr1","addr_info":[{"local":"198.51.100.7","prefixlen":24}]},
		{"ifname":"parnet","addr_info":[{"local":"10.251.0.1","prefixlen":22}]}]`
	s := settings.Default()
	// The worker subnet and the host's address on it aren't listed: the group drops them through SDN's own IP set.
	block, err := ti.blocked(context.Background(), &s)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.2.0/24", "192.0.2.5", "192.168.0.0/16", "198.51.100.7"}
	if !slices.Equal(block, want) {
		t.Errorf("block = %v", block)
	}
}

func TestWorkerRulesUseScopedReferences(t *testing.T) {
	rules := workerRules(netipMust("192.0.2.1"))
	var drops []string
	for _, r := range rules {
		if r.Action == "DROP" {
			drops = append(drops, r.Dest)
		}
	}
	if !slices.Equal(drops, []string{"+sdn/parnet-all", "+dc/par-blocked"}) {
		t.Errorf("drops = %v", drops)
	}
	// DNS to the host's resolver comes first, so it passes even when the resolver is on the LAN.
	if rules[0].Action != "ACCEPT" || rules[0].Dest != "192.0.2.1" || rules[0].Dport != "53" {
		t.Errorf("first rule = %+v", rules[0])
	}
}

func TestWaitBooted(t *testing.T) {
	ti := newTestInstaller(t)
	ti.node.vms[100] = &fakeVM{id: 100, running: true, config: map[string]string{}}
	must(t, ti.waitBooted(context.Background(), 100))

	ti.node.guest = func(*fakeVM, []string, []byte) (int, string, string, bool) { return 1, "degraded\n", "", true }
	must(t, ti.waitBooted(context.Background(), 100))
	if !strings.Contains(ti.stderr.String(), "finished booting with a failed unit") {
		t.Errorf("stderr = %s", ti.stderr)
	}

	ti.node.guest = func(*fakeVM, []string, []byte) (int, string, string, bool) { return 1, "starting\n", "", true }
	if err := ti.waitBooted(context.Background(), 100); err == nil || !strings.Contains(err.Error(), "state: starting") {
		t.Errorf("waitBooted = %v", err)
	}

	// A VM whose agent never answers gives up after 5 minutes of the fake clock.
	ti.node.vms[100].running = false
	if err := ti.waitAgent(context.Background(), 100); err == nil || !strings.Contains(err.Error(), "5 minutes") {
		t.Errorf("waitAgent = %v", err)
	}
}
