package installer

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// vmSpec is the controller VM's hardware and identity.
type vmSpec struct {
	name             string
	cores, memoryMiB int
	diskGiB          int
	tags             []string
	net0, ipconfig0  string
}

func (in *Installer) controllerSpec(s *settings.Settings) vmSpec {
	return vmSpec{name: "par-controller", cores: 2, memoryMiB: 2048,
		diskGiB: ControllerGiB, tags: []string{vmtags.Managed, vmtags.Controller},
		net0: netConfig(s), ipconfig0: ipConfig(s, s.Network.ControllerIP)}
}

// netConfig is a virtio NIC on the LAN bridge, with the VLAN tag if set.
func netConfig(s *settings.Settings) string {
	c := "virtio,bridge=" + s.Network.Bridge
	if s.Network.VLAN != 0 {
		c += ",tag=" + strconv.Itoa(s.Network.VLAN)
	}
	return c
}

// ipConfig is cloud-init's ipconfig for dhcp or a static address.
func ipConfig(s *settings.Settings, addr string) string {
	if addr == settings.DHCP {
		return "ip=dhcp"
	}
	return "ip=" + addr + ",gw=" + s.Network.LANGateway
}

// createSystemVM creates the controller VM from its image and starts it.
func (in *Installer) createSystemVM(ctx context.Context, vmid int, spec vmSpec, image, storage string) error {
	id := strconv.Itoa(vmid)
	args := []string{"create", id, "--name", spec.name, "--pool", SystemPool, "--memory", strconv.Itoa(spec.memoryMiB),
		"--cores", strconv.Itoa(spec.cores), "--cpu", "host", "--ostype", "l26", "--scsihw", "virtio-scsi-single",
		"--net0", spec.net0, "--agent", "enabled=1", "--onboot", "1", "--serial0", "socket", "--vga", "serial0",
		"--tags", strings.Join(spec.tags, ";")}
	for _, a := range [][]string{
		args,
		{"set", id, "--scsi0", storage + ":0,import-from=" + image},
		{"set", id, "--ide2", storage + ":cloudinit", "--boot", "order=scsi0", "--ipconfig0", spec.ipconfig0,
			"--ciupgrade", "0"},
		{"start", id},
	} {
		if err := in.run(ctx, "qm", a...); err != nil {
			return err
		}
	}
	return nil
}

// systemVMID returns a free VMID for the controller VM outside the workers' and templates' range: the
// one Proxmox suggests, or the first free ID above the range when that falls inside it.
func (in *Installer) systemVMID(ctx context.Context, s *settings.Settings) (int, error) {
	id, err := in.PVE.NextID(ctx)
	if err != nil {
		return 0, err
	}
	r := s.Proxmox.VMIDRange
	if !r.Contains(id) {
		return id, nil
	}
	vms, err := in.vms(ctx)
	if err != nil {
		return 0, err
	}
	id = r.End + 1
	for slices.ContainsFunc(vms, func(vm proxmox.VM) bool { return vm.VMID == id }) {
		id++
	}
	return id, nil
}

// existingSystemVM returns the controller VM, if it exists. One that a failed run left without its disk
// is destroyed, and one that is stopped is started.
func (in *Installer) existingSystemVM(ctx context.Context, tag string) (int, bool, error) {
	vm, ok, err := in.systemVM(ctx, tag)
	if err != nil || !ok {
		return 0, false, err
	}
	node, err := in.nodeName(ctx)
	if err != nil {
		return 0, false, err
	}
	cfg, err := in.PVE.VMConfig(ctx, node, vm.VMID)
	if err != nil {
		return 0, false, err
	}
	id := strconv.Itoa(vm.VMID)
	if _, ok := cfg["scsi0"]; !ok {
		return 0, false, in.run(ctx, "qm", "destroy", id, "--purge", "1")
	}
	if vm.Status != "running" {
		if err := in.run(ctx, "qm", "start", id); err != nil {
			return 0, false, err
		}
	}
	return vm.VMID, true, nil
}

// createController creates the controller VM unless it exists, waits for its address, and adds it to what workers
// must not reach.
func (in *Installer) createController(ctx context.Context, s *settings.Settings, image func() (string, error)) (
	int, netip.Addr, error,
) {
	in.Out.Step("Controller VM")
	vmid, ok, err := in.existingSystemVM(ctx, vmtags.Controller)
	if err != nil {
		return 0, netip.Addr{}, err
	}
	if !ok {
		if vmid, err = in.systemVMID(ctx, s); err != nil {
			return 0, netip.Addr{}, err
		}
		path, err := image()
		if err != nil {
			return 0, netip.Addr{}, err
		}
		if err := in.createSystemVM(ctx, vmid, in.controllerSpec(s), path, s.Proxmox.Storage); err != nil {
			return 0, netip.Addr{}, err
		}
	}
	if err := in.waitBooted(ctx, vmid); err != nil {
		return 0, netip.Addr{}, err
	}
	addr, err := in.vmIPv4(ctx, vmid)
	if err != nil {
		return 0, netip.Addr{}, fmt.Errorf("the controller VM got no IPv4 address: %w", err)
	}
	in.Out.Say("    controller %d at %s", vmid, addr)
	// The firewall was set up before the controller had an address; block it now.
	return vmid, addr, in.configureFirewall(ctx, s, addr)
}
