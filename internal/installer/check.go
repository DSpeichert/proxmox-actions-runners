package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// ErrChecksFailed means at least one check failed; the checks said which.
var ErrChecksFailed = errors.New("checks failed")

// settingsOrDefault returns the host settings, or the default install's before an install.
func (in *Installer) settingsOrDefault() (*settings.Settings, error) {
	s, err := settings.Load(in.SettingsPath)
	if errors.Is(err, os.ErrNotExist) {
		d := settings.Default()
		return &d, nil
	}
	return s, err
}

// Check runs `parcon check`: before an install, every check an install runs first, for the default settings; after
// one, the node's checks and the controller's own.
func (in *Installer) Check(ctx context.Context) error {
	s, err := in.settingsOrDefault()
	if err != nil {
		return err
	}
	_, installed, err := in.InstalledRelease(ctx)
	if err != nil {
		return err
	}
	mode := ModeCheck
	if installed {
		mode = ModeInstalled
	}
	in.Out.Step("Node")
	checks := in.Preflight(ctx, s, mode)
	failed := false
	for _, c := range checks {
		in.Out.Say("%s", c)
		failed = failed || !c.OK
	}
	if installed {
		for _, target := range []string{"config", "proxmox", "github", "template"} {
			in.Out.Step("Controller: parcon check %s", target)
			if err := in.Forward(ctx, in.Out.W, in.Out.Err, "check", target); err != nil {
				failed = true
				if !errors.Is(err, ErrChecksFailed) {
					in.Out.Say("FAIL  %v", err)
				}
			}
		}
	}
	if failed {
		return ErrChecksFailed
	}
	in.Out.Say("\nAll checks passed.")
	return nil
}

// Forward runs a parcon command as the controller, in its sandbox and with its credentials, and passes on its
// output. A command that fails there fails here with ErrChecksFailed.
func (in *Installer) Forward(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	if _, err := os.Stat(in.UnitPath); err != nil {
		return errors.New("the controller isn't set up; run parcon install")
	}
	out, err := in.controllerExec(ctx, 2*time.Minute, args...)
	_, _ = stdout.Write(out)
	if err != nil {
		var exit *pvecli.ExitError
		if errors.As(err, &exit) {
			if exit.Stderr != "" {
				_, _ = fmt.Fprintln(stderr, exit.Stderr)
			}
			return ErrChecksFailed
		}
		return err
	}
	return nil
}

// CheckNetworkInstalled runs the worker network check on an installed node.
func (in *Installer) CheckNetworkInstalled(ctx context.Context) error {
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	return in.CheckNetwork(ctx, s)
}
