package installer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// newRelease makes ti the parcon of another release, run from where the user put it, as an update runs it.
func newRelease(t *testing.T, ti *testInstaller, version string) release.Version {
	t.Helper()
	v, err := release.ParseVersion(version)
	must(t, err)
	self := filepath.Join(ti.dir, "parcon-"+version)
	must(t, os.WriteFile(self, []byte("parcon "+version), 0o755))
	ti.Self, ti.Version, ti.Image, ti.Yes = self, v, "", true
	return v
}

// An update runs the new parcon: it installs itself and, given an image, imports it, then restarts the controller.
func TestUpdateToANewParconAndImage(t *testing.T) {
	ti := installed(t)
	newRelease(t, ti, "0.3.0")
	ti.Image = writeImage(t, filepath.Join(ti.dir, "new"), "0.3.0", "2.339.0")
	must(t, ti.Update(context.Background(), UpdateOptions{}))
	out := ti.stdout.String()
	for _, want := range []string{"install parcon 0.3.0 as " + ti.BinaryPath, "import " + ti.Image + " as a new " +
		"runner template", "render the controller's config", "record release 0.3.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	n := ti.node
	if data, _ := os.ReadFile(ti.BinaryPath); string(data) != "parcon 0.3.0" {
		t.Errorf("host binary = %q", data)
	}
	tmpl := n.vmsTagged("par-release-0.3.0")
	if len(tmpl) != 1 || !slices.Contains(n.vms[tmpl[0]].tags, "par-rv-2.339.0") {
		t.Errorf("templates of 0.3.0: %v", tmpl)
	}
	if !n.ran("qm set " + strconv.Itoa(tmpl[0]) + " --scsi0 local-lvm:0,import-from=" + ti.Image) {
		t.Errorf("the image wasn't imported from its path:\n%s", strings.Join(n.lines(), "\n"))
	}
	if s, _ := settings.Load(ti.SettingsPath); s.Installed != "0.3.0" {
		t.Errorf("installed = %q", s.Installed)
	}
	if !n.ran("systemctl try-restart parcon.service") {
		t.Errorf("the controller wasn't restarted: %v", n.lines())
	}

	// Once there, another update has nothing to do, and looks nothing up.
	n.calls = nil
	ti.stdout.Reset()
	ti.Self, ti.Image = ti.BinaryPath, ""
	must(t, ti.Update(context.Background(), UpdateOptions{}))
	if !strings.Contains(ti.stdout.String(), "nothing needs updating") || n.ran("pvesh create") {
		t.Errorf("second update:\n%s", ti.stdout)
	}
}

// Without an image, an update keeps the runner template, and says how to import a new one.
func TestUpdateWithoutAnImageKeepsTheTemplate(t *testing.T) {
	ti := installed(t)
	newRelease(t, ti, "0.3.0")
	templates := ti.node.vmsTagged(vmtags.Template)
	must(t, ti.Update(context.Background(), UpdateOptions{}))
	if !strings.Contains(ti.stdout.String(), "(the runner template stays; --image PATH imports a new one)") {
		t.Errorf("plan:\n%s", ti.stdout)
	}
	if now := ti.node.vmsTagged(vmtags.Template); !slices.Equal(now, templates) {
		t.Errorf("templates %v, then %v", templates, now)
	}
}

// The image's actions/runner version comes from its manifest, or from --runner-version without one.
func TestUpdateImageWithoutAManifest(t *testing.T) {
	ti := installed(t)
	v := newRelease(t, ti, "0.3.0")
	ti.Image = filepath.Join(ti.dir, "custom.qcow2")
	must(t, os.WriteFile(ti.Image, []byte("my image"), 0o644))
	err := ti.Update(context.Background(), UpdateOptions{})
	if err == nil || !strings.Contains(err.Error(), "custom.json") || !strings.Contains(err.Error(), "--runner-version") {
		t.Fatalf("Update = %v", err)
	}
	ti.RunnerVersion = "banana"
	if err := ti.Update(context.Background(), UpdateOptions{}); err == nil || !strings.Contains(err.Error(), "banana") {
		t.Errorf("Update with a bad runner version = %v", err)
	}
	ti.RunnerVersion = "2.340.0"
	must(t, ti.Update(context.Background(), UpdateOptions{}))
	tmpl := ti.node.vmsTagged("par-release-" + v.String())
	if len(tmpl) != 1 || !slices.Contains(ti.node.vms[tmpl[0]].tags, "par-rv-2.340.0") {
		t.Errorf("templates of %s: %v", v, tmpl)
	}
}

func TestUpgradeRefusesADowngrade(t *testing.T) {
	ti := installed(t)
	newer, _ := release.ParseVersion("0.9.0")
	if err := ti.Upgrade(context.Background(), newer, true, false); err == nil || !strings.Contains(err.Error(), "newer than") {
		t.Errorf("Upgrade = %v", err)
	}
}

func TestInstallOnAnOlderInstallUpdatesIt(t *testing.T) {
	ti := installed(t)
	s, err := settings.Load(ti.SettingsPath)
	must(t, err)
	s.Installed = "0.1.0"
	must(t, s.Save(ti.SettingsPath))
	ti.Yes = true
	must(t, ti.Install(context.Background(), defaultOptions()))
	if !strings.Contains(ti.stdout.String(), "Update proxmox-actions-runners 0.1.0 to 0.2.0") {
		t.Errorf("output:\n%s", ti.stdout)
	}
	if s, _ := settings.Load(ti.SettingsPath); s.Installed != "0.2.0" {
		t.Errorf("installed = %q", s.Installed)
	}
	if !ti.node.ran("systemctl try-restart parcon.service") {
		t.Error("the controller wasn't restarted with the new release")
	}
}

// A node with an older release's controller VM can't be updated in place.
func TestUpgradeRefusesAControllerVMInstall(t *testing.T) {
	ti := installed(t)
	ti.node.vms[100] = &fakeVM{id: 100, name: "par-controller", pool: SystemPool,
		tags: []string{vmtags.Managed, vmtags.Controller, "par-release-0.1.0"}, running: true,
		config: map[string]string{}}
	v, _ := release.ParseVersion("0.2.0")
	err := ti.Upgrade(context.Background(), v, true, false)
	if err == nil || !strings.Contains(err.Error(), "VM 100 (par-controller) is from an older release") {
		t.Errorf("Upgrade = %v", err)
	}
	d := settings.Default()
	checks := ti.Preflight(context.Background(), &d, ModeInstall)
	if !slices.ContainsFunc(checks, func(c Check) bool {
		return c.Desc == "no controller VM from an older release" && !c.OK
	}) {
		t.Errorf("preflight passes a controller VM: %v", checks)
	}
}
