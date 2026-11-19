package settings

import (
	"errors"
	"os"
	"path/filepath"
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
	s.GitHub = GitHub{ConfigURL: "https://github.com/my-org", App: App{ClientID: "Iv23liEXAMPLE", InstallationID: 7}}
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
	if got.GitHub != s.GitHub || got.Worker != s.Worker || got.ScaleSet.MaxLifetime != 36*time.Hour || strings.Join(got.ScaleSet.Labels, ",") != "null,true" {
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
	s.GitHub.ConfigURL = "https://github.com/my-org/my-repo"
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
		"proxmox.storage", "proxmox.vmidRange", "scaleSet.name", "scaleSet.labels", "scaleSet.runnerGroup",
		"scaleSet.maxLifetime", "scaleSet.minRunners", "worker.memoryMiB",
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
