package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// managedVMs returns the VMs tagged Managed in our pools: clones before templates, since Proxmox won't destroy a
// template that linked clones still use.
func managedVMs(vms []proxmox.VM) []proxmox.VM {
	var out []proxmox.VM
	for _, vm := range vms {
		if (vm.Pool == SystemPool || vm.Pool == RunnerPool) && vm.HasTag(vmtags.Managed) {
			out = append(out, vm)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Template != out[j].Template {
			return !out[i].Template
		}
		return out[i].VMID < out[j].VMID
	})
	return out
}

func vmids(vms []proxmox.VM) string {
	ids := make([]string, len(vms))
	for i, vm := range vms {
		ids[i] = strconv.Itoa(vm.VMID)
	}
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, " ")
}

// Uninstall removes everything parcon and the controller created: the scale sets in GitHub, the VMs (an older
// release's gateway VM too), the Proxmox objects, the worker firewall and network, the offload rule, no-kvm-apf,
// the settings, and parcon itself, last. The GitHub Apps stay.
func (in *Installer) Uninstall(ctx context.Context) error {
	vms, err := in.vms(ctx)
	if err != nil {
		return err
	}
	managed := managedVMs(vms)
	controller, hasController, err := in.systemVM(ctx, vmtags.Controller)
	if err != nil {
		return err
	}
	node, err := in.nodeName(ctx)
	if err != nil {
		return err
	}

	in.Out.Step("Plan")
	in.Out.Say("Remove proxmox-actions-runners from node %s:", node)
	if hasController {
		targets := "every target"
		if s, err := settings.Load(in.SettingsPath); err == nil && len(s.GitHub.Targets) > 0 {
			targets = strings.Join(targetURLs(s), ", ")
		}
		in.Out.Say("  stop the controller and delete its scale sets in GitHub, for %s (the Apps themselves stay)",
			targets)
	}
	in.Out.Say("  destroy the VMs tagged %s in %s and %s, now: %s", vmtags.Managed, SystemPool, RunnerPool,
		vmids(managed))
	in.Out.Say("  remove pools %s and %s, role %s, user %s with its token and ACLs", SystemPool, RunnerPool, Role,
		PVEUser)
	in.Out.Say("  remove firewall security group %s, IP set %s, and the host's DHCP rule for %s (the datacenter "+
		"firewall stays on)", FirewallGroup, BlockedIPSet, VNet)
	in.Out.Say("  remove SDN subnet, VNet %s, and zone %s, then apply the SDN config", VNet, Zone)
	if line := in.offloadRemovePlan(); line != "" {
		in.Out.Say("  %s", line)
	}
	if line := in.asyncPFRemovePlan(); line != "" {
		in.Out.Say("  %s", line)
	}
	in.Out.Say("  remove %s and %s", in.SettingsPath, in.BinaryPath)
	if in.Change.DryRun {
		return nil
	}
	if err := in.confirm("Uninstall with this plan? This can't be undone."); err != nil {
		return err
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	if hasController && controller.Status == "running" {
		in.Out.Step("Stop the controller")
		in.Change.Log(fmt.Sprintf("controller VM %d: systemctl disable --now parcon.service", controller.VMID))
		if _, err := in.guestExec(ctx, controller.VMID, ControllerStopTimeout, nil, "systemctl", "disable", "--now",
			"parcon.service"); err != nil {
			in.Out.Warn("stopping parcon.service failed: %v", err)
		}
		in.Change.Log(fmt.Sprintf("controller VM %d: parcon github scaleset delete", controller.VMID))
		if out, err := in.asParcon(ctx, controller.VMID, 2*time.Minute, nil, "parcon", "github", "scaleset",
			"delete"); err != nil {
			in.Out.Warn("deleting the scale sets in GitHub failed; remove those left in the organizations' or "+
				"repositories' Actions settings (Runners): %v", err)
		} else {
			in.Out.Say("    %s", strings.TrimSpace(string(out)))
		}
	}

	in.Out.Step("Destroy VMs")
	// Listed again: until it stopped, the controller kept creating and destroying workers.
	if vms, err = in.vms(ctx); err != nil {
		return err
	}
	for _, vm := range managedVMs(vms) {
		if err := in.destroyVM(ctx, vm.VMID); err != nil {
			return err
		}
	}

	if err := in.removeAccess(ctx); err != nil {
		return err
	}
	if err := in.removeFirewall(ctx); err != nil {
		return err
	}
	if err := in.removeNetwork(ctx); err != nil {
		return err
	}
	if err := in.removeOffloads(ctx); err != nil {
		return err
	}
	if err := in.removeAsyncPF(ctx); err != nil {
		return err
	}

	in.Out.Step("parcon on the host")
	if err := in.Change.Remove(in.SettingsPath); err != nil {
		return err
	}
	// The directory goes too, if nothing else is in it.
	if err := os.Remove(filepath.Dir(in.SettingsPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		in.Out.Say("    kept %s: it isn't empty", filepath.Dir(in.SettingsPath))
	}
	// Linux lets a running program remove its own file.
	if err := in.Change.Remove(in.BinaryPath); err != nil {
		return err
	}
	in.Out.Step("Done")
	in.Out.Say("proxmox-actions-runners is removed. The GitHub Apps remain; delete them in GitHub's settings if you like.")
	return nil
}

func (in *Installer) removeAccess(ctx context.Context) error {
	in.Out.Step("Proxmox access objects")
	pools, err := in.PVE.Pools(ctx)
	if err != nil {
		return err
	}
	for _, p := range pools {
		if p.ID == SystemPool || p.ID == RunnerPool {
			if err := in.run(ctx, "pveum", "pool", "delete", p.ID); err != nil {
				in.Out.Warn("pool %s isn't empty: it holds VMs parcon didn't create", p.ID)
			}
		}
	}
	users, err := in.PVE.Users(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u != PVEUser {
			continue
		}
		acls, err := in.PVE.ACLs(ctx)
		if err != nil {
			return err
		}
		for _, a := range acls {
			if a.UGID == PVEUser || strings.HasPrefix(a.UGID, PVEUser+"!") {
				if err := in.run(ctx, "pveum", "acl", "delete", a.Path, "--"+a.Type+"s", a.UGID, "--roles",
					a.Role); err != nil {
					return err
				}
			}
		}
		if tokens, err := in.PVE.Tokens(ctx, PVEUser); err == nil && len(tokens) > 0 {
			for _, t := range tokens {
				if t == TokenName {
					if err := in.run(ctx, "pveum", "user", "token", "remove", PVEUser, TokenName); err != nil {
						return err
					}
				}
			}
		}
		if err := in.run(ctx, "pveum", "user", "delete", PVEUser); err != nil {
			return err
		}
	}
	roles, err := in.PVE.Roles(ctx)
	if err != nil {
		return err
	}
	for _, r := range roles {
		if r.ID == Role {
			return in.run(ctx, "pveum", "role", "delete", Role)
		}
	}
	return nil
}

func (in *Installer) removeNetwork(ctx context.Context) error {
	in.Out.Step("Worker network")
	changed := false
	// A VNet with a subnet can't be removed.
	if subnets, err := in.PVE.Subnets(ctx, VNet); err == nil {
		for _, sn := range subnets {
			if err := in.run(ctx, "pvesh", "delete", "/cluster/sdn/vnets/"+VNet+"/subnets/"+sn.ID); err != nil {
				return err
			}
			changed = true
		}
	}
	for _, obj := range []string{"vnets/" + VNet, "zones/" + Zone} {
		if in.PVE.SDNExists(ctx, obj) {
			if err := in.run(ctx, "pvesh", "delete", "/cluster/sdn/"+obj); err != nil {
				return err
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	pending, err := in.PVE.PendingSDN(ctx, Zone, VNet)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		// Applying would also apply someone else's unfinished SDN changes.
		in.Out.Warn("not applying the SDN config: %s have pending changes too. Review them, then apply the SDN "+
			"config (Datacenter > SDN > Apply, or pvesh set /cluster/sdn) to remove %s and %s from the host.",
			strings.Join(pending, " "), VNet, Zone)
		return nil
	}
	return in.run(ctx, "pvesh", "set", "/cluster/sdn")
}
