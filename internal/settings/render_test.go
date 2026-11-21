package settings

import (
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
)

var testEnv = Env{
	Node:        "pve1",
	PVEAddress:  netip.MustParseAddr("192.0.2.5"),
	TLS:         hostsys.TLSInfo{Mode: hostsys.TLSNodeCA, ServerName: "pve1"},
	LinkedClone: true,
}

func TestControllerConfigBeforeTheApp(t *testing.T) {
	s := Default()
	c, data, err := ControllerConfig(&s, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Proxmox
	if p.URL != "https://192.0.2.5:8006/api2/json" || p.TokenID != "par@pve!controller" || p.Node != "pve1" ||
		p.CACertFile != "${CREDENTIALS_DIRECTORY}/parcon.pve-ca.pem" || p.TokenSecretFile != "${CREDENTIALS_DIRECTORY}/parcon.pve-token" || p.TLSServerName != "pve1" || p.Pool != "par-runners" ||
		p.VNet != "parnet" || !p.UseLinkedClone() || p.VMIDRange != (config.VMIDRange{Start: 10000, End: 10099}) {
		t.Errorf("proxmox = %+v", p)
	}
	if strings.Contains(string(data), "github") {
		t.Errorf("config has a github section before the App exists:\n%s", data)
	}
	if err := c.RequireGitHub(); err == nil {
		t.Error("RequireGitHub passes without an App")
	}
	ss := c.ScaleSets[0]
	if ss.Name != config.DefaultScaleSetName || ss.MaxRunners != 1 || ss.Worker != config.DefaultWorker() {
		t.Errorf("scale set = %+v", ss)
	}
	if !strings.HasPrefix(string(data), "# Written by parcon") {
		t.Errorf("no header:\n%s", data)
	}
}

func TestControllerConfigWithTheApp(t *testing.T) {
	s := Default()
	s.GitHub = GitHub{
		Apps: []App{{ClientID: "Iv23liEXAMPLE"}, {ClientID: "Iv23liUSER"}},
		Targets: []Target{{URL: "https://github.com/my-org", ClientID: "Iv23liEXAMPLE", InstallationID: 7},
			// Waiting for its installation: the controller doesn't serve it yet.
			{URL: "https://github.com/octocat/hello", ClientID: "Iv23liUSER"}},
	}
	s.ScaleSet.Labels = []string{"null", "true", "linux"}
	s.ScaleSet.MaxRunners = 4
	s.Worker.MemoryMiB = 16384
	env := testEnv
	env.TLS = hostsys.TLSInfo{Mode: hostsys.TLSPin, Fingerprint: "AA:BB"}
	env.LinkedClone = false
	c, _, err := ControllerConfig(&s, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RequireGitHub(); err != nil {
		t.Error(err)
	}
	wantGitHub := config.GitHub{
		Apps: []config.GitHubApp{{ClientID: "Iv23liEXAMPLE", PrivateKeyFile: config.CredentialPath(config.CredAppKey("Iv23liEXAMPLE"))},
			{ClientID: "Iv23liUSER", PrivateKeyFile: config.CredentialPath(config.CredAppKey("Iv23liUSER"))}},
		Targets: []config.Target{{ConfigURL: "https://github.com/my-org", ClientID: "Iv23liEXAMPLE", InstallationID: 7}},
	}
	if !reflect.DeepEqual(c.GitHub, wantGitHub) {
		t.Errorf("github = %+v, want %+v", c.GitHub, wantGitHub)
	}
	if c.MaxWorkers != 4 {
		t.Errorf("maxWorkers = %d, want runners.max", c.MaxWorkers)
	}
	if c.Proxmox.TLSFingerprint != "AA:BB" || c.Proxmox.CACertFile != "" || c.Proxmox.UseLinkedClone() {
		t.Errorf("proxmox = %+v", c.Proxmox)
	}
	ss := c.ScaleSets[0]
	if !slices.Equal(ss.Labels, []string{"null", "true", "linux"}) || ss.MaxRunners != 4 ||
		ss.Worker.MemoryMiB != 16384 || ss.Worker.Cores != config.DefaultCores {
		t.Errorf("scale set = %+v", ss)
	}
}

func TestControllerConfigRefusesWhatTheControllerWould(t *testing.T) {
	s := Default()
	s.ScaleSet.MaxRunners = 200 // more than the VMID range holds
	if _, _, err := ControllerConfig(&s, testEnv); err == nil || !strings.Contains(err.Error(), "vmidRange") {
		t.Errorf("err = %v", err)
	}
}

func TestControllerConfigMaxLifetime(t *testing.T) {
	s := Default()
	c, data, err := ControllerConfig(&s, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	// The default is left to the controller's config, the one home of defaults.
	if c.ScaleSets[0].MaxLifetime != config.DefaultMaxLifetime || strings.Contains(string(data), "maxLifetime") {
		t.Errorf("default: %s\n%s", c.ScaleSets[0].MaxLifetime, data)
	}
	s.ScaleSet.MaxLifetime = 36 * time.Hour
	if c, _, err = ControllerConfig(&s, testEnv); err != nil || c.ScaleSets[0].MaxLifetime != 36*time.Hour {
		t.Errorf("36h: %v, %v", c.ScaleSets[0].MaxLifetime, err)
	}
}
