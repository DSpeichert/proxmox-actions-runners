package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/controller"
	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// StatusSchema is the version of `parcon status --json`'s format. Version 3 reports the controller's service, where
// version 2 reported its VM.
const StatusSchema = 3

// StatusLevel is how a status line is judged.
type StatusLevel string

// Status levels.
const (
	OK   StatusLevel = "ok"
	WARN StatusLevel = "warn"
	FAIL StatusLevel = "fail"
)

// Finding is one judged line of the status.
type Finding struct {
	Level StatusLevel `json:"level"`
	Text  string      `json:"text"`
}

// Report is the state of the whole install, for `parcon status`.
type Report struct {
	Schema     int    `json:"schema"`
	Version    string `json:"version"`
	Node       string `json:"node"`
	PVEVersion string `json:"pveVersion"`
	// Installed is the release of the finished install; empty if the node has none.
	Installed string `json:"installed,omitempty"`

	Host       []Finding                   `json:"host"`
	Controller *ControllerStatus           `json:"controller,omitempty"`
	GitHub     []Finding                   `json:"github"`
	ScaleSets  []controller.ScaleSetStatus `json:"scaleSets,omitempty"`
	Templates  []TemplateStatus            `json:"templates"`
	Workers    []WorkerStatus              `json:"workers"`
	MaxRunners int                         `json:"maxRunners"`
	Settings   []SettingStatus             `json:"settings,omitempty"`
	// SettingsWarnings are the settings that are allowed but likely to cause trouble on this node.
	SettingsWarnings []string `json:"settingsWarnings,omitempty"`
}

// ControllerStatus is the controller's service.
type ControllerStatus struct {
	// State is parcon.service's state: active, inactive, failed, and so on.
	State    string    `json:"state"`
	Findings []Finding `json:"findings"`
}

// TemplateStatus is a runner template.
type TemplateStatus struct {
	VMID          int       `json:"vmid"`
	RunnerVersion string    `json:"runnerVersion"`
	ImportedAt    time.Time `json:"importedAt"`
	Newest        bool      `json:"newest"`
}

// WorkerStatus is a worker VM.
type WorkerStatus struct {
	VMID      int       `json:"vmid"`
	Name      string    `json:"name"`
	ScaleSet  string    `json:"scaleSet"`
	Running   bool      `json:"running"`
	Ready     bool      `json:"ready"`
	CreatedAt time.Time `json:"createdAt"`
}

// SettingStatus is a config key's value.
type SettingStatus struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	IsDefault bool   `json:"isDefault"`
}

// Failed reports whether anything in the report failed.
func (r *Report) Failed() bool {
	all := slices.Concat(r.Host, r.GitHub)
	if r.Controller != nil {
		all = append(all, r.Controller.Findings...)
	}
	return slices.ContainsFunc(all, func(f Finding) bool { return f.Level == FAIL })
}

// githubTimeout bounds each GitHub lookup status makes: the answer is useful, but not worth a long wait.
const githubTimeout = 10 * time.Second

// Status reports the state of the whole install. It says at once that it is collecting, then writes a line to
// the error stream for each part as it comes in, since the parts that ask the VMs and GitHub take a few seconds.
// They run at the same time.
func (in *Installer) Status(ctx context.Context) (*Report, error) {
	var progressMu sync.Mutex
	progress := func(format string, args ...any) {
		progressMu.Lock()
		defer progressMu.Unlock()
		_, _ = fmt.Fprintf(in.Out.Err, format+"\n", args...)
	}
	progress("Collecting the status of each part:")

	r := &Report{Schema: StatusSchema, Version: in.Version.String()}
	began := time.Now()
	var err error
	if r.Node, err = in.nodeName(ctx); err != nil {
		return nil, err
	}
	r.PVEVersion, _ = in.Sys.PVEVersion(ctx)
	if v, ok, err := in.InstalledRelease(ctx); err != nil {
		return nil, err
	} else if ok {
		r.Installed = v.String()
	}
	s, err := settings.Load(in.SettingsPath)
	if err != nil {
		return nil, fmt.Errorf("%w; run parcon install", err)
	}
	r.MaxRunners = s.ScaleSet.MaxRunners
	for _, k := range settings.Keys {
		v, isDefault := k.Get(s)
		r.Settings = append(r.Settings, SettingStatus{Key: k.Name, Value: v, IsDefault: isDefault})
	}
	for _, w := range settings.Warnings(s, in.Limits()) {
		r.SettingsWarnings = append(r.SettingsWarnings, w.Text)
	}
	vms, err := in.vms(ctx)
	if err != nil {
		return nil, err
	}
	r.Templates, r.Workers = templatesAndWorkers(vms)
	progress("  %s  node %s, its settings, and its VMs (%s)", levelLabel(OK), r.Node, since(began))

	var wg sync.WaitGroup
	part := func(name string, fn func() StatusLevel) {
		wg.Go(func() {
			began := time.Now()
			level := fn()
			progress("  %s  %s (%s)", levelLabel(level), name, since(began))
		})
	}
	part("the host's Proxmox objects and tuning", func() StatusLevel {
		r.Host = in.hostFindings(ctx, s)
		return worst(r.Host)
	})
	var snapshot *controller.Status
	part("the controller's service", func() StatusLevel {
		r.Controller, snapshot = in.controllerStatus(ctx, s)
		return worst(r.Controller.Findings)
	})
	var runner Finding
	part("the runner template against actions/runner's latest release", func() StatusLevel {
		ctx, cancel := context.WithTimeout(ctx, githubTimeout)
		defer cancel()
		runner = in.runnerFinding(ctx, r.Templates)
		return runner.Level
	})
	wg.Wait()
	progress("")

	for _, tag := range []string{vmtags.Controller, vmtags.Gateway} {
		if vm, ok := findSystemVM(vms, tag); ok {
			r.Host = append(r.Host, Finding{FAIL, fmt.Sprintf("VM %d (%s) is from an older release, which this "+
				"release can't update; run parcon uninstall with that release, then parcon install", vm.VMID,
				vm.Name)})
		}
	}
	r.GitHub = append(githubFindings(s, snapshot, in.Now()), runner)
	if snapshot != nil {
		r.ScaleSets = snapshot.ScaleSets
	}
	return r, nil
}

// worst is the most serious level among findings.
func worst(findings []Finding) StatusLevel {
	level := OK
	for _, f := range findings {
		switch f.Level {
		case FAIL:
			return FAIL
		case WARN:
			level = WARN
		}
	}
	return level
}

func levelLabel(l StatusLevel) string {
	return map[StatusLevel]string{OK: "ok  ", WARN: "WARN", FAIL: "FAIL"}[l]
}

// since is how long ago began was, to a tenth of a second.
func since(began time.Time) string { return time.Since(began).Round(100 * time.Millisecond).String() }

func findSystemVM(vms []proxmox.VM, tag string) (proxmox.VM, bool) {
	t := tagged(vms, SystemPool, tag)
	if len(t) == 0 {
		return proxmox.VM{}, false
	}
	return t[0], true
}

func (in *Installer) hostFindings(ctx context.Context, s *settings.Settings) []Finding {
	f := []Finding{{OK, fmt.Sprintf("parcon %s at %s, settings at %s", in.Version, in.BinaryPath, in.SettingsPath)}}
	var missing []string
	pools, _ := in.PVE.Pools(ctx)
	if !slices.ContainsFunc(pools, func(x pvecli.Pool) bool { return x.ID == RunnerPool }) {
		missing = append(missing, "pool "+RunnerPool)
	}
	if roles, _ := in.PVE.Roles(ctx); !slices.ContainsFunc(roles, func(r pvecli.Role) bool { return r.ID == Role }) {
		missing = append(missing, "role "+Role)
	}
	if tokens, _ := in.PVE.Tokens(ctx, PVEUser); !slices.Contains(tokens, TokenName) {
		missing = append(missing, "token "+PVEUser+"!"+TokenName)
	}
	for _, obj := range []string{"zones/" + Zone, "vnets/" + VNet} {
		if !in.PVE.SDNExists(ctx, obj) {
			missing = append(missing, "SDN "+obj)
		}
	}
	if len(missing) > 0 {
		f = append(f, Finding{FAIL, "missing " + strings.Join(missing, ", ") + "; run parcon install"})
	} else {
		f = append(f, Finding{OK, fmt.Sprintf("pools, role, token, and the worker network %s/%s", Zone, VNet)})
	}
	f = append(f, in.networkFindings(ctx, s)...)
	for _, c := range []struct {
		name string
		r    result
	}{
		{"LAN NIC offloads", in.checkOffloads(ctx, s, ModeInstalled)},
		{"KVM async page faults", in.checkAsyncPF(ctx, ModeInstalled)},
	} {
		level := OK
		if !c.r.ok {
			level = WARN
		}
		f = append(f, Finding{level, c.name + ": " + c.r.reason})
	}
	return f
}

// networkFindings judges the worker network and firewall: without the datacenter firewall, workers reach the LAN.
func (in *Installer) networkFindings(ctx context.Context, s *settings.Settings) []Finding {
	var f []Finding
	if on, err := in.PVE.ClusterFirewall(ctx); err != nil || !on {
		f = append(f, Finding{FAIL, "the datacenter firewall is off: workers can reach the LAN and the host. " +
			"Turn it on under Datacenter > Firewall > Options"})
	}
	if drift := append(in.networkDrift(ctx, s), in.firewallDrift(ctx, s)...); len(drift) > 0 {
		f = append(f, Finding{FAIL, strings.Join(drift, "; ") + "; run parcon update to correct it"})
	} else {
		f = append(f, Finding{OK, fmt.Sprintf("worker subnet %s with SNAT and DHCP; firewall group %s and IP set %s",
			s.Network.WorkerSubnet, FirewallGroup, BlockedIPSet)})
	}
	if !in.Sys.ServiceActive(ctx, "dnsmasq@"+Zone+".service") {
		f = append(f, Finding{FAIL, "dnsmasq@" + Zone + " isn't active: workers get no DHCP lease"})
	}
	return f
}

// controllerStatus reports the controller's service: whether it runs, whether its unit, config, and credentials
// are what the settings want, and what it last reported.
func (in *Installer) controllerStatus(ctx context.Context, s *settings.Settings) (*ControllerStatus,
	*controller.Status) {
	st := &ControllerStatus{State: in.serviceState(ctx)}
	add := func(level StatusLevel, format string, args ...any) {
		st.Findings = append(st.Findings, Finding{level, fmt.Sprintf(format, args...)})
	}
	if _, err := os.Stat(in.UnitPath); err != nil {
		add(FAIL, "%s isn't set up; run parcon install", ServiceName)
		return st, nil
	}
	if st.State == "active" {
		add(OK, "%s is active", ServiceName)
	} else {
		add(FAIL, "%s is %s; see journalctl -u parcon", ServiceName, st.State)
	}
	if in.unitDrift() {
		add(WARN, "%s isn't the unit this parcon writes; run parcon config apply", in.UnitPath)
	}
	if want, _, err := in.RenderConfig(ctx, s); err != nil {
		add(FAIL, "the settings don't render a config: %v", err)
	} else if have, err := os.ReadFile(in.ConfigPath); err != nil {
		add(FAIL, "can't read the controller's config: %v", err)
	} else if string(have) != string(want) {
		add(WARN, "the controller's config doesn't match the settings; run parcon config apply")
	} else {
		add(OK, "its config matches the settings")
	}
	missing := []string{}
	if !in.hasCredential(config.CredToken) {
		missing = append(missing, "the Proxmox API token")
	}
	for _, a := range s.GitHub.Apps {
		if !in.hasCredential(config.CredAppKey(a.ClientID)) {
			missing = append(missing, "GitHub App "+appName(a)+"'s key")
		}
	}
	if len(missing) > 0 {
		add(FAIL, "missing from %s: %s", in.CredStoreDir, strings.Join(missing, ", "))
	} else {
		add(OK, "its secrets are encrypted in %s", in.CredStoreDir)
	}

	snap := in.readStatus()
	if snap == nil {
		return st, nil
	}
	if age := in.Now().Sub(snap.UpdatedAt); age > 2*time.Minute {
		add(WARN, "its last pass was %s ago", ago(age))
	}
	if snap.LastError != "" {
		add(WARN, "its last pass failed: %s", snap.LastError)
	}
	return st, snap
}

// readStatus reads the status file the controller writes after each pass, or nil if there is none it can read.
func (in *Installer) readStatus() *controller.Status {
	data, err := os.ReadFile(in.StatusPath)
	if err != nil {
		return nil
	}
	var snap controller.Status
	if json.Unmarshal(data, &snap) != nil || snap.Schema != controller.StatusSchema {
		return nil
	}
	return &snap
}

func githubFindings(s *settings.Settings, snap *controller.Status, now time.Time) []Finding {
	if len(s.GitHub.Targets) == 0 {
		return []Finding{{FAIL, "the GitHub App isn't set up yet; run parcon install"}}
	}
	var f []Finding
	for _, t := range s.GitHub.Targets {
		if !t.Complete() {
			f = append(f, Finding{FAIL, fmt.Sprintf("%s: its App isn't installed yet; run parcon target add "+
				"--github-url %s", t.URL, t.URL)})
			continue
		}
		f = append(f, Finding{OK, fmt.Sprintf("%s, App %s, installation %d", t.URL, t.ClientID, t.InstallationID)})
		if snap == nil {
			continue
		}
		for _, ss := range snap.ScaleSets {
			if ss.TargetKey != t.Key() {
				continue
			}
			f = append(f, scaleSetFindings(ss, now)...)
		}
	}
	if snap == nil {
		f = append(f, Finding{WARN, "the controller reports no status yet"})
	}
	return f
}

func scaleSetFindings(ss controller.ScaleSetStatus, now time.Time) []Finding {
	var f []Finding
	switch {
	case ss.SessionOpen:
		f = append(f, Finding{OK, fmt.Sprintf("  scale set %s (ID %d): listening for jobs for %s", ss.Name, ss.ID,
			ago(now.Sub(ss.SessionSince)))})
	case ss.SessionError != "":
		f = append(f, Finding{FAIL, fmt.Sprintf("  scale set %s: no session with GitHub since %s ago: %s", ss.Name,
			ago(now.Sub(ss.SessionSince)), ss.SessionError)})
	default:
		f = append(f, Finding{WARN, fmt.Sprintf("  scale set %s: no session with GitHub yet", ss.Name)})
	}
	if st := ss.Stats; st != nil {
		f = append(f, Finding{OK, fmt.Sprintf("    jobs: %d waiting, %d running; runners: %d (%d busy, %d idle)",
			st.AvailableJobs+st.AcquiredJobs, st.RunningJobs, st.RegisteredRunners, st.BusyRunners, st.IdleRunners)})
	}
	return f
}

func templatesAndWorkers(vms []proxmox.VM) ([]TemplateStatus, []WorkerStatus) {
	var templates []TemplateStatus
	newest, hasNewest := vmtags.NewestTemplate(vms, RunnerPool)
	for _, vm := range vms {
		if vmtags.IsTemplate(vm, RunnerPool) {
			rv, _ := vmtags.String(vm, vmtags.RunnerVersionPrefix)
			tv, _ := vmtags.Int(vm, vmtags.TemplateVersionPrefix)
			templates = append(templates, TemplateStatus{VMID: vm.VMID, RunnerVersion: rv,
				ImportedAt: time.Unix(tv, 0).UTC(), Newest: hasNewest && vm.VMID == newest.VMID})
		}
	}
	workers := []WorkerStatus{}
	for _, vm := range vms {
		if vm.Pool != RunnerPool || !vm.HasTag(vmtags.Worker) {
			continue
		}
		created, _ := vmtags.Int(vm, vmtags.CreatedPrefix)
		ss, _ := vmtags.String(vm, vmtags.ScaleSetPrefix)
		workers = append(workers, WorkerStatus{VMID: vm.VMID, Name: vm.Name, ScaleSet: ss,
			Running: vm.Status == "running", Ready: vm.HasTag(vmtags.Ready), CreatedAt: time.Unix(created, 0).UTC()})
	}
	return templates, workers
}

// latestRunnerRelease is actions/runner's newest release. Tests replace it.
var latestRunnerRelease = github.LatestRunnerRelease

func (in *Installer) runnerFinding(ctx context.Context, templates []TemplateStatus) Finding {
	var newest *TemplateStatus
	for i := range templates {
		if templates[i].Newest {
			newest = &templates[i]
		}
	}
	if newest == nil {
		return Finding{FAIL, "there is no runner template; run parcon update"}
	}
	rel, err := latestRunnerRelease(ctx)
	if err != nil {
		return Finding{WARN, fmt.Sprintf("template %d has actions/runner %s; the latest release isn't known: %v",
			newest.VMID, newest.RunnerVersion, err)}
	}
	text := fmt.Sprintf("template %d has actions/runner %s, the latest", newest.VMID, newest.RunnerVersion)
	switch rel.Staleness(newest.RunnerVersion, in.Now()) {
	case github.Current:
		return Finding{OK, text}
	case github.BehindError:
		return Finding{FAIL, fmt.Sprintf("template %d has actions/runner %s, but %s came out %s ago: GitHub stops "+
			"accepting it %s after that; run parcon update", newest.VMID, newest.RunnerVersion, rel.Version,
			ago(in.Now().Sub(rel.PublishedAt)), ago(github.RunnerUpdateDeadline))}
	default:
		return Finding{WARN, fmt.Sprintf("template %d has actions/runner %s; %s is out: run parcon update when a "+
			"release carries it", newest.VMID, newest.RunnerVersion, rel.Version)}
	}
}

// ago prints a duration the way status shows ages: 45s, 12m, 3h, 4d.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Render prints the report for people.
func (r *Report) Render(w io.Writer, now time.Time) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }
	line := func(f Finding) {
		label := map[StatusLevel]string{OK: "ok   ", WARN: "WARN ", FAIL: "FAIL "}[f.Level]
		p("  %s %s", label, f.Text)
	}
	installed := r.Installed
	if installed == "" {
		installed = "not finished"
	}
	p("proxmox-actions-runners %s on %s (%s)", installed, r.Node, r.PVEVersion)
	p("\nHost")
	for _, f := range r.Host {
		line(f)
	}
	if r.Controller != nil {
		p("\nController (%s)", ServiceName)
		for _, f := range r.Controller.Findings {
			line(f)
		}
	}
	p("\nGitHub")
	for _, f := range r.GitHub {
		line(f)
	}
	ready := 0
	for _, wk := range r.Workers {
		if wk.Ready {
			ready++
		}
	}
	p("\nWorkers: %d of at most %d, %d ready", len(r.Workers), r.MaxRunners, ready)
	for _, wk := range r.Workers {
		state := "stopped"
		switch {
		case wk.Running && wk.Ready:
			state = "ready"
		case wk.Running:
			state = "booting"
		}
		p("  %-6d %-8s %-4s old  %s", wk.VMID, state, ago(now.Sub(wk.CreatedAt)), wk.Name)
	}
	p("\nSettings (parcon config get --all)")
	for _, s := range r.Settings {
		def := ""
		if s.IsDefault {
			def = " (default)"
		}
		p("  %-19s %s%s", s.Key, s.Value, def)
	}
	for _, w := range r.SettingsWarnings {
		p("  WARN  %s", w)
	}
}
