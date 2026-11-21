package installer

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// ErrUnchanged is a config set to the value it already has.
var ErrUnchanged = errors.New("unchanged")

// Settings returns the host settings.
func (in *Installer) Settings() (*settings.Settings, error) { return in.loadSettings() }

// SetConfig sets a config key and applies it: the controller gets the new config and restarts. Running workers keep going; a restarted controller adopts them. The
// settings change only once the controller has the new config. An invalid value is a *settings.ValueError.
func (in *Installer) SetConfig(ctx context.Context, key, value string) error {
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	k, err := settings.Lookup(key)
	if err != nil {
		return err
	}
	before, _ := k.Get(s)
	if err := settings.SetKey(s, key, value, in.Limits()); err != nil {
		return err
	}
	after, _ := k.Get(s)
	if after == before {
		in.Out.Say("%s is already %s", key, after)
		return ErrUnchanged
	}
	// Only what this key has a part in: another key's standing warning isn't news here. parcon status shows them all.
	for _, w := range settings.Warnings(s, in.Limits()) {
		if w.About(key) {
			in.Out.Warn("%s", w)
		}
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	in.Out.Say("%s: %s -> %s, which applies %s", key, before, after, k.Applies)
	return in.apply(ctx, s)
}

// ApplyConfig renders the controller's config from the settings again and restarts it, for a controller whose config
// drifted from the settings, or after the API's pinned certificate was renewed.
func (in *Installer) ApplyConfig(ctx context.Context) error {
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	return in.apply(ctx, s)
}

// apply saves the settings and, once the controller is set up, writes its config and restarts it.
func (in *Installer) apply(ctx context.Context, s *settings.Settings) error {
	if _, err := os.Stat(in.UnitPath); err != nil {
		// Before the controller is set up, the settings alone change; install brings the config along.
		in.Out.Warn("the controller isn't set up yet: the settings are saved, and the controller gets them from " +
			"parcon install")
		return in.saveSettings(s)
	}
	if err := in.writeConfig(ctx, s); err != nil {
		return err
	}
	if err := in.saveSettings(s); err != nil {
		return fmt.Errorf("the controller has the new config, but saving the settings failed; run parcon config "+
			"apply once it is fixed: %w", err)
	}
	return in.restartController(ctx, s)
}
