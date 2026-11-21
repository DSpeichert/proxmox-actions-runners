package installer

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/controller"
	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// installed returns a test installer on a node with a finished install.
func installed(t *testing.T) *testInstaller {
	t.Helper()
	ti := newTestInstaller(t)
	ti.answerManifest()
	must(t, ti.Install(context.Background(), defaultOptions()))
	ti.stdout.Reset()
	ti.stderr.Reset()
	ti.node.calls = nil
	return ti
}

func TestSetConfig(t *testing.T) {
	ti := installed(t)
	ctx := context.Background()
	must(t, ti.SetConfig(ctx, "worker.memory", "16GiB"))

	s, err := settings.Load(ti.SettingsPath)
	if err != nil || s.Worker.MemoryMiB != 16384 {
		t.Fatalf("settings = %+v, %v", s, err)
	}
	cfg := ti.controllerConfig(t)
	if cfg.ScaleSets[0].Worker.MemoryMiB != 16384 {
		t.Errorf("controller config: %+v", cfg)
	}
	for _, want := range []string{"systemctl try-restart parcon.service", "journalctl -u parcon.service"} {
		if !ti.node.ran(want) {
			t.Errorf("didn't run %s", want)
		}
	}
	if out := ti.stdout.String(); !strings.Contains(out, "worker.memory: 8GiB -> 16GiB") ||
		!strings.Contains(out, "listening for jobs") {
		t.Errorf("output:\n%s", out)
	}

	// The same value again changes nothing.
	ti.node.calls = nil
	if err := ti.SetConfig(ctx, "worker.memory", "16384MiB"); !errors.Is(err, ErrUnchanged) || len(ti.node.calls) > 5 {
		t.Errorf("unchanged: %v, calls %v", err, ti.node.lines())
	}

	// A refused value is a ValueError and changes nothing.
	var verr *settings.ValueError
	if err := ti.SetConfig(ctx, "worker.memory", "16GB"); !errors.As(err, &verr) {
		t.Errorf("16GB: %v", err)
	}
	if err := ti.SetConfig(ctx, "worker.gpu", "1"); !errors.Is(err, settings.ErrUnknownKey) {
		t.Errorf("unknown key: %v", err)
	}

	// Too much memory for the node is allowed, with a warning.
	must(t, ti.SetConfig(ctx, "runners.max", "8"))
	if !strings.Contains(ti.stderr.String(), "more than this node's") {
		t.Errorf("stderr:\n%s", ti.stderr)
	}
	// A key the warning isn't about doesn't repeat it; one it is about does.
	ti.stderr.Reset()
	must(t, ti.SetConfig(ctx, "worker.cores", "4"))
	if strings.Contains(ti.stderr.String(), "more than this node's") {
		t.Errorf("setting worker.cores warned about memory:\n%s", ti.stderr)
	}
	must(t, ti.SetConfig(ctx, "worker.memory", "9GiB"))
	if !strings.Contains(ti.stderr.String(), "more than this node's") {
		t.Errorf("setting worker.memory didn't warn:\n%s", ti.stderr)
	}
}

// Before the controller is set up, a setting only changes the settings.
func TestSetConfigBeforeTheController(t *testing.T) {
	ti := installed(t)
	must(t, os.Remove(ti.UnitPath))
	must(t, ti.SetConfig(context.Background(), "runners.max", "2"))
	if s, _ := settings.Load(ti.SettingsPath); s.ScaleSet.MaxRunners != 2 {
		t.Error("the settings didn't change")
	}
	if ti.controllerConfig(t).MaxWorkers != 1 || ti.node.ran("systemctl try-restart") {
		t.Error("the controller changed")
	}
}

func TestStatus(t *testing.T) {
	ti := installed(t)
	latestRunnerRelease = func(context.Context) (github.RunnerRelease, error) {
		return github.RunnerRelease{Version: "2.338.0"}, nil
	}
	t.Cleanup(func() { latestRunnerRelease = github.LatestRunnerRelease })
	desired := 1
	snap := controller.Status{Schema: controller.StatusSchema, Version: "0.2.0", UpdatedAt: ti.Now(),
		ScaleSets: []controller.ScaleSetStatus{{Target: "https://github.com/my-org", TargetKey: "my-org",
			Name: "proxmox-ubuntu-26.04", ID: 3, SessionOpen: true,
			SessionSince: ti.Now().Add(-2 * time.Hour), Desired: &desired,
			Stats: &github.Stats{AvailableJobs: 1, RunningJobs: 1, RegisteredRunners: 1, BusyRunners: 1}}}}
	must(t, os.WriteFile(ti.StatusPath, []byte(jsonOf(snap)), 0o600))
	ti.node.vms[10000] = &fakeVM{id: 10000, name: "par-proxmox-ubuntu-26-04-abcde", pool: RunnerPool, running: true,
		tags: []string{"par-managed", "par-worker", "par-ready", "par-ss-proxmox-ubuntu-26.04",
			"par-created-" + strconv.FormatInt(ti.Now().Add(-12*time.Minute).Unix(), 10)}, config: map[string]string{}}

	r, err := ti.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Failed() {
		t.Errorf("a healthy install fails: %+v", r)
	}
	// The progress comes first, on the error stream, a line per part.
	progress := ti.stderr.String()
	if !strings.HasPrefix(progress, "Collecting the status of each part:\n") {
		t.Errorf("progress:\n%s", progress)
	}
	for _, want := range []string{"ok    node pve1, its settings, and its VMs (",
		"ok    the controller's service (",
		"ok    the host's Proxmox objects and tuning (",
		"ok    the runner template against actions/runner's latest release ("} {
		if !strings.Contains(progress, want) {
			t.Errorf("progress lacks %q:\n%s", want, progress)
		}
	}
	var out strings.Builder
	r.Render(&out, ti.Now())
	for _, want := range []string{
		"proxmox-actions-runners 0.2.0 on pve1",
		"ok    pools, role, token, and the worker network parzone/parnet",
		"ok    worker subnet 10.251.0.0/22 with SNAT and DHCP; firewall group par-worker and IP set par-blocked",
		"Controller (parcon.service)", "parcon.service is active", "its config matches the settings",
		"its secrets are encrypted in",
		"scale set proxmox-ubuntu-26.04 (ID 3): listening for jobs for 2h",
		"jobs: 1 waiting, 1 running; runners: 1 (1 busy, 0 idle)",
		"template 10096 has actions/runner 2.338.0, the latest",
		"Workers: 1 of at most 1, 1 ready", "10000  ready    12m",
		"worker.memory       8GiB (default)", "worker.maxLifetime  6h (default)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}

	// A standing settings warning shows up under Settings.
	s, _ := settings.Load(ti.SettingsPath)
	s.ScaleSet.MaxRunners = 9
	must(t, s.Save(ti.SettingsPath))
	r, _ = ti.Status(context.Background())
	out.Reset()
	r.Render(&out, ti.Now())
	if !strings.Contains(out.String(), "WARN  runners.max 9 × worker.memory 8GiB is 72GiB, more than this node's") {
		t.Errorf("status:\n%s", out.String())
	}
	s.ScaleSet.MaxRunners = 1
	must(t, s.Save(ti.SettingsPath))

	// A datacenter firewall that was turned off, and SDN's DHCP that stopped, fail.
	ti.node.clusterFW = false
	ti.node.services["dnsmasq@parzone.service"] = false
	r, _ = ti.Status(context.Background())
	out.Reset()
	r.Render(&out, ti.Now())
	if !r.Failed() || !strings.Contains(out.String(), "FAIL  the datacenter firewall is off") ||
		!strings.Contains(out.String(), "FAIL  dnsmasq@parzone isn't active") {
		t.Errorf("status:\n%s", out.String())
	}
	ti.node.clusterFW = true
	ti.node.services["dnsmasq@parzone.service"] = true

	// A stopped controller, a config that drifted, and a stale runner fail or warn.
	must(t, os.WriteFile(ti.ConfigPath, []byte("# edited by hand\n"), 0o600))
	latestRunnerRelease = func(context.Context) (github.RunnerRelease, error) {
		return github.RunnerRelease{Version: "2.339.0", PublishedAt: ti.Now().Add(-25 * 24 * time.Hour)}, nil
	}
	r, _ = ti.Status(context.Background())
	out.Reset()
	r.Render(&out, ti.Now())
	if !r.Failed() || !strings.Contains(out.String(), "WARN  the controller's config doesn't match the settings") ||
		!strings.Contains(out.String(), "FAIL  template 10096 has actions/runner 2.338.0, but 2.339.0 came out 25d ago") {
		t.Errorf("status:\n%s", out.String())
	}
}

func TestSetConfigMaxLifetime(t *testing.T) {
	ti := installed(t)
	must(t, ti.SetConfig(context.Background(), "worker.maxLifetime", "36h"))
	if cfg := ti.controllerConfig(t); cfg.ScaleSets[0].MaxLifetime != 36*time.Hour {
		t.Fatalf("controller config: %+v", cfg)
	}
	if !ti.node.ran("systemctl try-restart parcon.service") {
		t.Error("the controller wasn't restarted")
	}
	if out := ti.stdout.String(); !strings.Contains(out, "worker.maxLifetime: 6h -> 36h, which applies to every "+
		"worker, running ones included") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSetConfigDisk(t *testing.T) {
	ti := installed(t)
	must(t, ti.SetConfig(context.Background(), "worker.disk", "50GiB"))
	if cfg := ti.controllerConfig(t); cfg.ScaleSets[0].Worker.FreeDiskGiB != 50 {
		t.Errorf("controller config worker = %+v", cfg.ScaleSets[0].Worker)
	}
	if out := ti.stdout.String(); !strings.Contains(out, "worker.disk: 14GiB -> 50GiB") ||
		!strings.Contains(out, "listening for jobs") {
		t.Errorf("output:\n%s", out)
	}
}
