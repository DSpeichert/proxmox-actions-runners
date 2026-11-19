package settings

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	s := Default()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if w := s.EffectiveWorker(); w.Cores != 2 || w.MemoryMiB != 8192 || w.FreeDiskGiB != 14 {
		t.Errorf("EffectiveWorker = %+v", w)
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "settings.yaml")
	s := Default()
	s.GitHub = GitHub{
		Apps:    []App{{ClientID: "Iv23liEXAMPLE", Slug: "my-runners", Owner: "my-org"}},
		Targets: []Target{{URL: "https://github.com/my-org", ClientID: "Iv23liEXAMPLE", InstallationID: 7}},
	}
	s.ScaleSet.Labels = []string{"null", "true"}
	s.ScaleSet.MaxLifetime = 36 * time.Hour
	s.Worker.MemoryMiB = 16384
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", st.Mode(), err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.GitHub, s.GitHub) || got.Worker != s.Worker || got.ScaleSet.MaxLifetime != 36*time.Hour || strings.Join(got.ScaleSet.Labels, ",") != "null,true" {
		t.Errorf("Load = %+v", got)
	}
}

func TestParseRejects(t *testing.T) {
	for name, doc := range map[string]string{
		"empty":         "",
		"old version":   "version: 0\n",
		"unknown field": "version: 2\nnetwork: {bridge: vmbr0, controllerIP: dhcp, workerSubnet: 10.251.0.0/22}\nsurprise: 1\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseRefusesTheGatewayVMSettings(t *testing.T) {
	doc := "version: 1\nnetwork: {bridge: vmbr0, gatewayIP: dhcp, controllerIP: dhcp, workerSubnet: 10.251.0.0/22}\n"
	if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), "parcon uninstall, then parcon install") {
		t.Errorf("Parse = %v, want the reinstall message", err)
	}
}

func TestValidateChecksTheDHCPRange(t *testing.T) {
	s := Default()
	s.Network.WorkerSubnet = "10.251.0.0/29" // 5 leases
	s.ScaleSet.MaxRunners = 2
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "has 5 DHCP addresses but needs 6") {
		t.Errorf("Validate = %v", err)
	}
	s.ScaleSet.MaxRunners = 1
	if err := s.Validate(); err != nil {
		t.Errorf("Validate = %v", err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	s := Default()
	s.Network.Bridge = "vmbr0; rm"
	s.Network.VLAN = 5000
	s.Network.ControllerIP = "banana"
	s.Network.WorkerSubnet = "10.251.1.0/22"
	s.Proxmox.Storage = "-bad"
	s.Proxmox.VMIDRange.End = s.Proxmox.VMIDRange.Start + 2
	s.GitHub = GitHub{
		Apps: []App{{ClientID: "not an ID"}},
		Targets: []Target{{URL: "https://ghes.example.com/my-org"},
			{URL: "https://github.com/octocat/hello", ClientID: "Iv23liNONE"}},
	}
	s.ScaleSet.Name = "Ubuntu"
	s.ScaleSet.Labels = []string{"ok", "not ok"}
	s.ScaleSet.RunnerGroup = "big ones"
	s.ScaleSet.MinRunners = 3
	s.Worker.MemoryMiB = 512
	s.ScaleSet.MaxLifetime = time.Minute
	var verr *ValidationError
	if err := s.Validate(); !errors.As(err, &verr) {
		t.Fatalf("Validate = %v", err)
	}
	want := []string{
		"network.bridge", "network.vlan", "network.controllerIP", "network.lanGateway", "network.workerSubnet",
		"proxmox.storage", "proxmox.vmidRange", "github.apps[0].clientId", "github.targets[0].url",
		"github.targets[1].clientId", "scaleSet.name", "scaleSet.labels", "scaleSet.maxLifetime",
		"scaleSet.minRunners", "worker.memoryMiB",
	}
	if len(verr.Problems) != len(want) {
		t.Errorf("problems:\n%s", strings.Join(verr.Problems, "\n"))
	}
	for i, field := range want {
		if i < len(verr.Problems) && !strings.HasPrefix(verr.Problems[i], field+": ") {
			t.Errorf("problem %d = %q, want %s", i, verr.Problems[i], field)
		}
	}
}

// Settings from before several targets are read as one App and one target, and saved in the current format.
func TestParseMigratesVersion2(t *testing.T) {
	v2 := `version: 2
network: {bridge: vmbr0, controllerIP: dhcp, workerSubnet: 10.251.0.0/22}
proxmox: {storage: local-lvm, vmidRange: {start: 10000, end: 10099}}
github:
  configUrl: https://github.com/octocat/hello
  app: {clientId: Iv23liEXAMPLE, installationId: 7, slug: my-runners}
scaleSet: {name: proxmox-ubuntu-26.04, runnerGroup: default, minRunners: 0, maxRunners: 2}
`
	s, err := Parse([]byte(v2))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := GitHub{
		Apps:    []App{{ClientID: "Iv23liEXAMPLE", Slug: "my-runners", Owner: "octocat"}},
		Targets: []Target{{URL: "https://github.com/octocat/hello", ClientID: "Iv23liEXAMPLE", InstallationID: 7}},
	}
	if s.Version != Version || s.MigratedFrom() != 2 || !reflect.DeepEqual(s.GitHub, want) ||
		s.ScaleSet.MaxRunners != 2 {
		t.Errorf("Parse = %+v (migrated from %d)", s, s.MigratedFrom())
	}
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(data)
	if err != nil || again.MigratedFrom() != 0 || !reflect.DeepEqual(again.GitHub, want) {
		t.Errorf("Parse(Marshal) = %+v, %v\n%s", again, err, data)
	}

	// An install that stopped before its App was installed: only the target, or only the App.
	for doc, want := range map[string]GitHub{
		"configUrl: https://github.com/my-org": {Targets: []Target{{URL: "https://github.com/my-org"}}},
		"app: {clientId: Iv23liEXAMPLE}":       {Apps: []App{{ClientID: "Iv23liEXAMPLE"}}},
	} {
		s, err := Parse([]byte(strings.Replace(v2, `  configUrl: https://github.com/octocat/hello
  app: {clientId: Iv23liEXAMPLE, installationId: 7, slug: my-runners}`, "  "+doc, 1)))
		if err != nil || !reflect.DeepEqual(s.GitHub, want) {
			t.Errorf("Parse with %s = %+v, %v", doc, s, err)
		}
	}
}

func TestGitHubLookups(t *testing.T) {
	g := GitHub{
		Apps: []App{{ClientID: "Iv23liORG", Owner: "My-Org"}, {ClientID: "Iv23liUSER"}},
		Targets: []Target{{URL: "https://github.com/my-org", ClientID: "Iv23liORG", InstallationID: 1},
			{URL: "https://github.com/octocat/hello", ClientID: "Iv23liUSER", InstallationID: 2},
			{URL: "https://github.com/other"}},
	}
	if a := g.AppFor("my-org"); a == nil || a.ClientID != "Iv23liORG" {
		t.Errorf("AppFor(my-org) = %+v, want the App it owns", a)
	}
	// An App without a known owner is found through the targets it serves.
	if a := g.AppFor("Octocat"); a == nil || a.ClientID != "Iv23liUSER" {
		t.Errorf("AppFor(Octocat) = %+v, want the App of its target", a)
	}
	if a := g.AppFor("other"); a != nil {
		t.Errorf("AppFor(other) = %+v, want none: its target has no App yet", a)
	}
	if tg := g.Target("https://github.com/OCTOCAT/hello/"); tg == nil || tg.InstallationID != 2 {
		t.Errorf("Target = %+v", tg)
	}
	if got := g.CompleteTargets(); len(got) != 2 {
		t.Errorf("CompleteTargets = %+v", got)
	}
	if got := g.TargetsOf("Iv23liUSER"); !reflect.DeepEqual(got, []string{"https://github.com/octocat/hello"}) {
		t.Errorf("TargetsOf = %v", got)
	}
}

// An App has one installation per account, shared by the account's repositories.
func TestValidateInstallationsPerAccount(t *testing.T) {
	s := Default()
	s.GitHub = GitHub{
		Apps: []App{{ClientID: "Iv23liUSER"}},
		Targets: []Target{{URL: "https://github.com/octocat/hello", ClientID: "Iv23liUSER", InstallationID: 2},
			{URL: "https://github.com/octocat/world", ClientID: "Iv23liUSER", InstallationID: 3}},
	}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "differs from 2") {
		t.Errorf("Validate = %v", err)
	}
	s.GitHub.Targets[1].InstallationID = 2
	if err := s.Validate(); err != nil {
		t.Errorf("Validate = %v", err)
	}
}
