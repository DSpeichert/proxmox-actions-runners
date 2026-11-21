package main

import (
	"bytes"
	"flag"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

func parseSettingsFlags(t *testing.T, args ...string) (*settings.Settings, *settingsFlags, error) {
	t.Helper()
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var sf settingsFlags
	sf.register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	s, err := sf.settings(fs, settings.Limits{HostCPUs: 8, HostMemMiB: 65536})
	return s, &sf, err
}

func TestSettingsFlags(t *testing.T) {
	s, sf, err := parseSettingsFlags(t)
	if err != nil || sf.given {
		t.Fatalf("defaults: %v, given %v", err, sf.given)
	}
	if d := settings.Default(); s.Network != d.Network || s.Proxmox != d.Proxmox || s.ScaleSet.Name != d.ScaleSet.Name {
		t.Errorf("defaults = %+v", s)
	}

	s, sf, err = parseSettingsFlags(t, "--bridge", "vmbr1", "--pve-address", "192.0.2.5", "--vmid-range", "20000-20099",
		"--scale-set", "big", "--labels", "big,linux",
		"--github-url", "https://github.com/my-org/", "--set", "runners.max=4", "--set", "worker.memory=16GiB",
		"--app", "manual", "--client-id", "Iv23liEXAMPLE")
	if err != nil || !sf.given {
		t.Fatalf("flags: %v, given %v", err, sf.given)
	}
	if s.Network.Bridge != "vmbr1" || s.Network.PVEAddress != "192.0.2.5" || s.Proxmox.VMIDRange != (config.VMIDRange{Start: 20000, End: 20099}) ||
		!slices.Equal(s.ScaleSet.Labels, []string{"big", "linux"}) || s.ScaleSet.MaxRunners != 4 ||
		s.Worker.MemoryMiB != 16384 || !slices.Equal(s.GitHub.Targets, []settings.Target{{URL: "https://github.com/my-org"}}) || sf.clientID != "Iv23liEXAMPLE" {
		t.Errorf("settings = %+v", s)
	}

	// --app and --client-id alone aren't settings.
	if _, sf, _ := parseSettingsFlags(t, "--app", "manual", "--client-id", "x"); sf.given {
		t.Error("--app counts as a setting")
	}
}

func TestSettingsFlagsRefuse(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--vmid-range", "10000"}, "FIRST-LAST"},
		{[]string{"--set", "runners.max"}, "KEY=VALUE"},
		{[]string{"--set", "worker.memory=16GB"}, "use GiB or MiB"},
		{[]string{"--set", "worker.gpu=1"}, "unknown key"},
		{[]string{"--set", "worker.disk=1"}, "needs a number and a unit"},
		{[]string{"--app", "browser"}, "--app must be manifest or manual"},
		{[]string{"--pve-address", "pve1"}, "network.pveAddress"},
	} {
		if _, _, err := parseSettingsFlags(t, tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want %q", tt.args, err, tt.want)
		}
	}
}

func TestCommandsRunWhereTheyBelong(t *testing.T) {
	defer func(f func() bool) { inService = f }(inService)
	var stdout, stderr bytes.Buffer

	inService = func() bool { return true }
	for _, cmd := range []string{"install", "status", "update", "config", "uninstall", "target"} {
		if err := run([]string{cmd}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "runs on the Proxmox host") {
			t.Errorf("%s in the sandbox: %v", cmd, err)
		}
	}

	inService = func() bool { return false }
	for _, cmd := range []string{"run", "github"} {
		if err := run([]string{cmd}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "runs in the controller's sandbox") {
			t.Errorf("%s on the host: %v", cmd, err)
		}
	}
	stdout.Reset()
	if err := run([]string{"help"}, &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), "on the Proxmox host") {
		t.Errorf("help on the host: %v\n%s", err, stdout.String())
	}
	if err := run([]string{"frobnicate"}, &stdout, &stderr); outcomeOf(err) != usageError {
		t.Errorf("unknown command on the host: %v", err)
	}
}
