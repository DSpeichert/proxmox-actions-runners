package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// UpdateOptions are the choices for an update.
type UpdateOptions struct {
	// Force redoes every piece of the update, even those already done, to repair an install that a stopped update
	// or anything else left broken.
	Force bool
}

// Update brings the install to this parcon: the user runs the new parcon, with --image for a new runner image, and
// it installs itself, imports the image, and does the rest. parcon looks up and downloads nothing. Running it again
// continues an update that stopped partway.
func (in *Installer) Update(ctx context.Context, opts UpdateOptions) error {
	installed, ok, err := in.InstalledRelease(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("there is no finished install on this node; run parcon install")
	}
	if in.Image != "" {
		if _, err := in.image(); err != nil {
			return err
		}
	}
	return in.Upgrade(ctx, installed, false, opts.Force)
}

// upgradeStep is one thing an upgrade does.
type upgradeStep struct {
	plan string
	do   func(ctx context.Context) error
}

// Upgrade brings the node from release installed to this parcon's release: parcon itself, the runner template when
// Image names a new one, the worker network and firewall, the controller's config and service, and the host's NIC
// offloads and KVM async page faults. Each piece already at this release is left alone, unless force redoes it.
func (in *Installer) Upgrade(ctx context.Context, installed release.Version, confirmed, force bool) error {
	if installed.Compare(in.Version) > 0 {
		return fmt.Errorf("the node runs release %s, newer than this parcon %s; run parcon update with the newer "+
			"parcon", installed, in.Version)
	}
	if err := in.refuseOlderLayout(ctx); err != nil {
		return err
	}
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	if on, err := in.PVE.ClusterFirewall(ctx); err != nil {
		return err
	} else if !on {
		return errNoClusterFirewall
	}
	steps, err := in.upgradeSteps(ctx, s, force)
	if err != nil {
		return err
	}
	in.Out.Step("Plan")
	if len(steps) == 0 {
		in.Out.Say("proxmox-actions-runners is at %s, and nothing needs updating. To import a new runner image, "+
			"run parcon update --image PATH.", in.Version)
		return nil
	}
	in.Out.Say("Update proxmox-actions-runners %s to %s:", installed, in.Version)
	for _, st := range steps {
		in.Out.Say("  %s", st.plan)
	}
	if in.Image == "" {
		in.Out.Say("  (the runner template stays; --image PATH imports a new one)")
	}
	if in.Change.DryRun {
		return nil
	}
	if !confirmed {
		if err := in.confirm("Update with this plan?"); err != nil {
			return err
		}
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	for _, st := range steps {
		if err := st.do(ctx); err != nil {
			return err
		}
	}
	in.Out.Step("Done")
	in.Out.Say("proxmox-actions-runners is at %s.", in.Version)
	in.rebootNote(ctx)
	return nil
}

// upgradeSteps works out what an upgrade to this release has to do. None means the node is at this release. A new
// runner image is imported whenever Image names one. With force, every other step runs, even one already done for
// this release: it sets up the worker network and firewall again, rewrites the controller's config and unit and
// restarts it, and runs the host tuning. The safety checks stay: the plan and its question, and the lock.
func (in *Installer) upgradeSteps(ctx context.Context, s *settings.Settings, force bool) ([]upgradeStep, error) {
	want, _, err := in.RenderConfig(ctx, s)
	if err != nil {
		return nil, err
	}
	have, err := os.ReadFile(in.ConfigPath)
	newBinary := !sameFile(in.Self, in.BinaryPath)
	// The controller is this parcon: a new release (which an update installed before it ran this one) or a new
	// binary needs a restart, as does a config or unit that differs.
	restart := newBinary || s.Installed != in.Version.String() || err != nil || !bytes.Equal(have, want) ||
		in.unitDrift()
	network := append(in.networkDrift(ctx, s), in.firewallDrift(ctx, s)...)
	offloads := in.offloadPlan(ctx, s)
	asyncPF := in.asyncPFPlan(ctx)

	var steps []upgradeStep
	if newBinary {
		steps = append(steps, upgradeStep{plan: fmt.Sprintf("install parcon %s as %s", in.Version, in.BinaryPath),
			do: func(context.Context) error { return in.installBinary() }})
	}
	if in.Image != "" {
		steps = append(steps, upgradeStep{
			plan: fmt.Sprintf("import %s as a new runner template; the controller removes old ones once unused",
				in.Image),
			do: func(ctx context.Context) error { return in.importTemplate(ctx, s, true) },
		})
	}
	if len(network) > 0 || force {
		plan := "check the worker network and firewall, and set them up again"
		if len(network) > 0 {
			plan = "correct the worker network and firewall: " + strings.Join(network, "; ")
		}
		steps = append(steps, upgradeStep{plan: plan, do: func(ctx context.Context) error {
			if err := in.createNetwork(ctx, s); err != nil {
				return err
			}
			return in.configureFirewall(ctx, s)
		}})
	}
	if restart || force {
		steps = append(steps, upgradeStep{
			plan: fmt.Sprintf("render the controller's config and %s from the settings, restart the controller "+
				"(running workers keep going), and wait for its sessions", ServiceName),
			do: func(ctx context.Context) error {
				in.Out.Step("Controller")
				if err := in.writeConfig(ctx, s); err != nil {
					return err
				}
				return in.restartController(ctx, s)
			},
		})
	}
	if offloads != nil || force {
		if offloads == nil {
			offloads = []string{"check the LAN NIC's offloads, and turn them off if the node is a VM"}
		}
		steps = append(steps, upgradeStep{plan: strings.Join(offloads, "\n  "),
			do: func(ctx context.Context) error { return in.tuneOffloads(ctx, s) }})
	}
	if asyncPF != nil || force {
		if asyncPF == nil {
			asyncPF = []string{"check KVM async page faults, and set no-kvm-apf if the node is a VM on KVM"}
		}
		steps = append(steps, upgradeStep{plan: strings.Join(asyncPF, "\n  "), do: in.tuneAsyncPF})
	}
	if len(steps) == 0 && s.Installed == in.Version.String() {
		return nil, nil
	}
	// The settings record the release last: it marks a finished update.
	return append(steps, upgradeStep{
		plan: fmt.Sprintf("record release %s in %s", in.Version, in.SettingsPath),
		do:   func(context.Context) error { return in.markInstalled(s) },
	}), nil
}

// sameFile reports whether two paths hold the same bytes: the host's parcon is this one.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	da, err1 := os.ReadFile(a) //nolint:gosec // G304: parcon's own paths.
	db, err2 := os.ReadFile(b) //nolint:gosec // G304: parcon's own paths.
	return err1 == nil && err2 == nil && bytes.Equal(da, db)
}
