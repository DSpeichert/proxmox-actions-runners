package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

func (in *Installer) run(ctx context.Context, name string, args ...string) error {
	_, err := in.Change.Run(ctx, pvecli.C(name, args...))
	return err
}

// privileges is the controller token's role: exactly what the controller checks for, on the runner pool, the
// storage, and the VNet.
func privileges() string {
	var privs []string
	for _, group := range proxmox.RequiredPrivileges(RunnerPool, "storage", Zone, VNet) {
		privs = append(privs, group.Privileges...)
	}
	slices.Sort(privs)
	return strings.Join(slices.Compact(privs), ",")
}

// createAccess creates the runner pool, the role, and the user. The pool comes first: its comment marks the install
// as ours.
func (in *Installer) createAccess(ctx context.Context) error {
	in.Out.Step("Proxmox access objects")
	pools, err := in.PVE.Pools(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(pools, func(p pvecli.Pool) bool { return p.ID == RunnerPool }) {
		if err := in.run(ctx, "pveum", "pool", "add", RunnerPool, "--comment", PoolComment); err != nil {
			return err
		}
	}
	roles, err := in.PVE.Roles(ctx)
	if err != nil {
		return err
	}
	verb := "add"
	if slices.ContainsFunc(roles, func(r pvecli.Role) bool { return r.ID == Role }) {
		verb = "modify"
	}
	if err := in.run(ctx, "pveum", "role", verb, Role, "--privs", privileges()); err != nil {
		return err
	}
	users, err := in.PVE.Users(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(users, PVEUser) {
		return in.run(ctx, "pveum", "user", "add", PVEUser, "--comment", "proxmox-actions-runners controller")
	}
	return nil
}

// grantACLs gives the user and its token the role on each path: a privilege-separated token gets only what both
// have.
func (in *Installer) grantACLs(ctx context.Context, s *settings.Settings) error {
	for _, path := range []string{"/pool/" + RunnerPool, "/storage/" + s.Proxmox.Storage,
		"/sdn/zones/" + Zone + "/" + VNet} {
		if err := in.run(ctx, "pveum", "acl", "modify", path, "--users", PVEUser, "--tokens",
			PVEUser+"!"+TokenName, "--roles", Role); err != nil {
			return err
		}
	}
	return nil
}

// createNetwork creates or corrects the worker network: a simple SDN zone with SDN's DHCP (dnsmasq) and IPAM, and a
// VNet with one subnet. The host takes the subnet's first address as the workers' gateway and SNATs their traffic;
// DHCP hands out the rest, with the host's DNS server. The worker firewall decides where that traffic may go.
func (in *Installer) createNetwork(ctx context.Context, s *settings.Settings) error {
	in.Out.Step("Worker network")
	w, err := workerNetwork(s)
	if err != nil {
		return err
	}
	dns, err := in.hostDNS(ctx, s)
	if err != nil {
		return err
	}
	changed := false
	if zone, err := in.PVE.SDNObject(ctx, "zones/"+Zone); err != nil {
		if err := in.run(ctx, "pvesh", "create", "/cluster/sdn/zones", "--type", "simple", "--zone", Zone,
			"--dhcp", "dnsmasq", "--ipam", "pve"); err != nil {
			return err
		}
		changed = true
	} else if zone["dhcp"] != "dnsmasq" || zone["ipam"] != "pve" {
		if err := in.run(ctx, "pvesh", "set", "/cluster/sdn/zones/"+Zone, "--dhcp", "dnsmasq", "--ipam", "pve"); err != nil {
			return err
		}
		changed = true
	}
	if !in.PVE.SDNExists(ctx, "vnets/"+VNet) {
		if err := in.run(ctx, "pvesh", "create", "/cluster/sdn/vnets", "--vnet", VNet, "--zone", Zone); err != nil {
			return err
		}
		changed = true
	}
	subnets, err := in.PVE.Subnets(ctx, VNet)
	if err != nil && !in.Change.DryRun {
		return err
	}
	args := []string{"--gateway", w.gateway.String(), "--snat", "1", "--dhcp-range", w.dhcpRange(),
		"--dhcp-dns-server", dns.String()}
	i := slices.IndexFunc(subnets, func(sn pvecli.Subnet) bool { return sn.CIDR == w.subnet.String() })
	switch {
	case i < 0:
		if err := in.run(ctx, "pvesh", append([]string{"create", "/cluster/sdn/vnets/" + VNet + "/subnets",
			"--type", "subnet", "--subnet", w.subnet.String()}, args...)...); err != nil {
			return err
		}
		changed = true
	case subnetDrift(subnets[i], w, dns) != "":
		if err := in.run(ctx, "pvesh", append([]string{"set", "/cluster/sdn/vnets/" + VNet + "/subnets/" +
			subnets[i].ID}, args...)...); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		if err := in.run(ctx, "pvesh", "set", "/cluster/sdn"); err != nil {
			return err
		}
	}
	if in.Change.DryRun {
		return nil
	}
	for deadline := in.Now().Add(time.Minute); !in.Sys.Exists(VNet); {
		if in.Now().After(deadline) {
			return fmt.Errorf("VNet %s didn't come up after applying the SDN config", VNet)
		}
		if err := in.Sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	return nil
}

// subnetDrift says how the worker subnet differs from what createNetwork sets up, or returns "".
func subnetDrift(sn pvecli.Subnet, w workerNet, dns netip.Addr) string {
	switch {
	case sn.Gateway != w.gateway.String():
		return "its gateway is " + sn.Gateway + ", not " + w.gateway.String()
	case sn.SNAT != 1:
		return "SNAT is off"
	case len(sn.DHCPRange) != 1 || sn.DHCPRange[0] != (pvecli.DHCPRange{Start: w.first.String(), End: w.last.String()}):
		return "its DHCP range isn't " + w.first.String() + "-" + w.last.String()
	case sn.DNSServer != dns.String():
		return "its DNS server is " + sn.DNSServer + ", not the host's " + dns.String()
	}
	return ""
}

// networkDrift returns what differs between the worker network and what createNetwork sets up, or nothing.
func (in *Installer) networkDrift(ctx context.Context, s *settings.Settings) []string {
	w, err := workerNetwork(s)
	if err != nil {
		return []string{err.Error()}
	}
	dns, err := in.hostDNS(ctx, s)
	if err != nil {
		return []string{err.Error()}
	}
	zone, err := in.PVE.SDNObject(ctx, "zones/"+Zone)
	if err != nil {
		return []string{"SDN zone " + Zone + " is missing"}
	}
	if zone["dhcp"] != "dnsmasq" || zone["ipam"] != "pve" {
		return []string{"SDN zone " + Zone + " doesn't use SDN DHCP"}
	}
	subnets, err := in.PVE.Subnets(ctx, VNet)
	if err != nil {
		return []string{"SDN VNet " + VNet + " is missing"}
	}
	i := slices.IndexFunc(subnets, func(sn pvecli.Subnet) bool { return sn.CIDR == w.subnet.String() })
	if i < 0 {
		return []string{"the worker subnet " + w.subnet.String() + " is missing"}
	}
	if d := subnetDrift(subnets[i], w, dns); d != "" {
		return []string{"the worker subnet: " + d}
	}
	return nil
}

// Asset names of a release.
func runnerImage(v release.Version) string    { return "par-runner-" + v.String() + ".qcow2" }
func runnerManifest(v release.Version) string { return "par-runner-" + v.String() + ".json" }

// BinaryAsset is the release's parcon binary.
func BinaryAsset(v release.Version) string { return "parcon-" + v.String() + "-linux-amd64" }

// assets returns this release's assets: the local directory given with Assets, or the signed release, downloaded
// into a temporary directory under DownloadDir that cleanup removes.
func (in *Installer) assets(ctx context.Context) (a *release.Assets, cleanup func(), err error) {
	if in.Assets != "" {
		a, err := release.Local(in.Assets, in.Version)
		return a, func() {}, err
	}
	dir, err := os.MkdirTemp(DownloadDir, "par-install.")
	if err != nil {
		return nil, nil, fmt.Errorf("make a download directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	a, err = release.Fetch(ctx, in.Source, in.Version, dir, in.Keys)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return a, cleanup, nil
}

// get returns a verified asset's path, saying so when it downloads one.
func (in *Installer) get(ctx context.Context, a *release.Assets, name string) (string, error) {
	in.Out.Say("    %s", name)
	return a.Get(ctx, name)
}

// runnerVersion reads the actions/runner version from the runner image's Packer manifest.
func runnerVersion(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: a verified asset.
	if err != nil {
		return "", err
	}
	var m struct {
		Builds []struct {
			CustomData struct {
				RunnerVersion string `json:"runner_version"`
			} `json:"custom_data"`
		} `json:"builds"`
	}
	if err := json.Unmarshal(data, &m); err != nil || len(m.Builds) == 0 {
		return "", fmt.Errorf("%s isn't a Packer manifest", path)
	}
	v := m.Builds[len(m.Builds)-1].CustomData.RunnerVersion
	if v == "" {
		return "", fmt.Errorf("%s names no runner version", path)
	}
	return v, nil
}

// freeReservedVMID returns the first unused VMID among the reserved IDs at the end of the range.
func freeReservedVMID(vms []proxmox.VM, r config.VMIDRange) (int, error) {
	for id := r.End - config.ReservedVMIDs + 1; id <= r.End; id++ {
		if !slices.ContainsFunc(vms, func(vm proxmox.VM) bool { return vm.VMID == id }) {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free VMID in the reserved IDs at the end of %d-%d", r.Start, r.End)
}

// importTemplate imports the runner image as a new template in the reserved VMIDs, unless one from this release
// exists and again is false. The template tags come first, so a failed import is found and replaced by the next
// run; the controller only clones a VM that Proxmox reports as a template, and removes older ones once unused.
func (in *Installer) importTemplate(ctx context.Context, s *settings.Settings, a *release.Assets, again bool) error {
	in.Out.Step("Runner template")
	vms, err := in.vms(ctx)
	if err != nil {
		return err
	}
	for _, t := range tagged(vms, RunnerPool, in.releaseTag()) {
		if t.Template {
			if !again {
				in.Out.Say("    template %d is from this release", t.VMID)
				return nil
			}
			// Workers may still use it: the controller removes it once they are gone.
			in.Out.Say("    template %d is from this release; importing a new one anyway", t.VMID)
			continue
		}
		in.Out.Say("    VM %d is a template import a failed run left; importing again", t.VMID)
		if err := in.run(ctx, "qm", "destroy", strconv.Itoa(t.VMID), "--purge", "1",
			"--destroy-unreferenced-disks", "1"); err != nil {
			return err
		}
		vms = slices.DeleteFunc(vms, func(vm proxmox.VM) bool { return vm.VMID == t.VMID })
	}
	image, err := in.get(ctx, a, runnerImage(in.Version))
	if err != nil {
		return err
	}
	manifest, err := in.get(ctx, a, runnerManifest(in.Version))
	if err != nil {
		return err
	}
	rv, err := runnerVersion(manifest)
	if err != nil {
		return err
	}
	vmid, err := freeReservedVMID(vms, s.Proxmox.VMIDRange)
	if err != nil {
		return err
	}
	id := strconv.Itoa(vmid)
	tags := strings.Join([]string{vmtags.Managed, vmtags.Template,
		vmtags.TemplateVersionPrefix + strconv.FormatInt(in.Now().Unix(), 10), vmtags.RunnerVersionPrefix + rv,
		in.releaseTag()}, ";")
	for _, args := range [][]string{
		{"create", id, "--name", "par-runner-" + strings.ReplaceAll(in.Version.String(), ".", "-"), "--pool",
			RunnerPool, "--memory", "2048", "--cores", "2", "--cpu", "host", "--ostype", "l26", "--scsihw",
			"virtio-scsi-single", "--net0", "virtio,bridge=" + VNet + ",firewall=1", "--agent", "enabled=1",
			"--serial0", "socket", "--vga", "serial0", "--tags", tags},
		{"set", id, "--scsi0", s.Proxmox.Storage + ":0,import-from=" + image},
		{"set", id, "--ide2", s.Proxmox.Storage + ":cloudinit", "--boot", "order=scsi0", "--ipconfig0", "ip=dhcp"},
	} {
		if err := in.run(ctx, "qm", args...); err != nil {
			return err
		}
	}
	// Before it becomes a template: clones copy its firewall, and the controller starts no worker without it.
	if err := in.templateFirewall(ctx, id); err != nil {
		return err
	}
	return in.run(ctx, "qm", "template", id)
}
