package installer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// TargetOptions says which organization or repository parcon target add adds, and how it gets an App for an account
// that has none yet.
type TargetOptions struct {
	// URL is https://github.com/<org> or https://github.com/<owner>/<repo>.
	URL string
	// App and ClientID are as for install: the manifest flow, or an existing App's Client ID.
	App      AppMode
	ClientID string
}

// AddTarget adds an organization or repository to a finished install. An account that already has an App reuses
// it, since a private App can only be installed on the account that owns it; otherwise the user creates one with the
// manifest flow, or imports an existing one. parcon waits for the App to be installed on the target, pushes the
// config, and restarts the controller, which registers the scale set there. A re-run continues a target whose App
// isn't installed yet.
func (in *Installer) AddTarget(ctx context.Context, opts TargetOptions) error {
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(opts.URL, "/")
	owner, repo, err := config.ParseGitHubURL(url)
	if err != nil {
		return err
	}
	if t := s.GitHub.Target(url); t != nil && t.Complete() {
		return fmt.Errorf("%s is already a target; parcon target list shows them", t.URL)
	}
	if err := in.finishedController(ctx); err != nil {
		return err
	}
	app := s.GitHub.AppFor(owner)
	if app == nil && opts.App == AppManual && opts.ClientID == "" {
		return errors.New("--app manual needs --client-id, the existing App's Client ID")
	}

	in.Out.Step("Plan")
	in.Out.Say("Add %s to the organizations and repositories the runners serve:", url)
	switch {
	case app != nil:
		in.Out.Say("  GitHub App: %s, which already serves %s; install it for %s if it isn't", appName(*app),
			strings.Join(s.GitHub.TargetsOf(app.ClientID), ", "), url)
	case opts.App == AppManual:
		in.Out.Say("  GitHub App: the existing App %s, its private key pasted here and encrypted for the "+
			"controller in %s", opts.ClientID, in.CredStoreDir)
	default:
		in.Out.Say("  GitHub App: a new one for %s, which you create in a browser; its key is encrypted for the "+
			"controller in %s", owner, in.CredStoreDir)
	}
	what := "the organization, for all its repositories"
	if repo != "" {
		what = "the repository"
	}
	in.Out.Say("  GitHub: scale set %s (runs-on %s) for %s", s.ScaleSet.Name, runsOn(s), what)
	in.Out.Say("  host: save the target in %s", in.SettingsPath)
	in.Out.Say("  controller: write its config and restart %s (running workers keep going), and wait for every "+
		"target's session", ServiceName)
	in.Out.Say("Workers: at most %d at once (runners.max), shared by all %d targets", s.ScaleSet.MaxRunners,
		len(s.GitHub.CompleteTargets())+1)
	if in.Change.DryRun {
		return nil
	}
	if err := in.confirm("Add the target?"); err != nil {
		return err
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	if s.GitHub.Target(url) == nil {
		s.GitHub.Targets = append(s.GitHub.Targets, settings.Target{URL: url})
		if err := s.Validate(); err != nil {
			return err
		}
		if err := in.saveSettings(s); err != nil {
			return err
		}
	}
	in.Out.Step("GitHub App")
	clientID, err := in.ensureApp(ctx, s, owner, opts.App, opts.ClientID)
	if err != nil {
		return err
	}
	if err := in.bindInstallation(ctx, s, clientID, url, "parcon target add --github-url "+url); err != nil {
		return err
	}
	if err := in.checkGitHub(ctx); err != nil {
		return err
	}
	if err := in.restartController(ctx, s); err != nil {
		return err
	}
	in.Out.Say("\nWorkflows in %s can use the runners now:\n\n  runs-on: %s", url, runsOn(s))
	return nil
}

// RemoveTarget removes an organization or repository: it deletes the target's scale sets in GitHub first, which
// unregisters their runners, then pushes the config without it and restarts the controller, which destroys the
// target's workers. The App's key goes too once no target uses the App; the App itself stays in GitHub. The last
// target can't be removed: parcon uninstall removes everything.
func (in *Installer) RemoveTarget(ctx context.Context, url string) error {
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	t := s.GitHub.Target(url)
	if t == nil {
		return fmt.Errorf("%s isn't a target; parcon target list shows them", url)
	}
	if len(s.GitHub.Targets) == 1 {
		return fmt.Errorf("%s is the only target; parcon uninstall removes the install", t.URL)
	}
	if err := in.finishedController(ctx); err != nil {
		return err
	}
	removed := *t
	appGoes := removed.ClientID != "" && len(s.GitHub.TargetsOf(removed.ClientID)) == 1
	vms, err := in.vms(ctx)
	if err != nil {
		return err
	}
	workers := targetWorkers(vms, removed.Key(), s.GitHub.Targets[0].Key() == removed.Key())

	in.Out.Step("Plan")
	in.Out.Say("Remove %s from the organizations and repositories the runners serve:", removed.URL)
	if removed.Complete() {
		in.Out.Say("  GitHub: delete scale set %s for %s, which unregisters its runners; a job running on one of them "+
			"fails", s.ScaleSet.Name, removed.URL)
	}
	in.Out.Say("  controller: write its config without it and restart %s, which destroys its workers, now: %s",
		ServiceName, vmids(workers))
	if appGoes {
		app := s.GitHub.AppByID(removed.ClientID)
		in.Out.Say("  controller: remove the key of App %s, which no other target uses, from %s (the App itself "+
			"stays in GitHub; delete it there if you like)", appName(*app), in.CredStoreDir)
	}
	in.Out.Say("  host: save the settings in %s", in.SettingsPath)
	if in.Change.DryRun {
		return nil
	}
	if err := in.confirm("Remove the target?"); err != nil {
		return err
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	// The scale sets go first, with the config that still names the target and its App; if GitHub refuses, nothing
	// else has changed, and a re-run tries again.
	if removed.Complete() {
		in.Change.Log("as the controller: parcon github scaleset delete -target " + removed.URL)
		out, err := in.controllerExec(ctx, 2*time.Minute, "github", "scaleset", "delete", "-target", removed.URL)
		if err != nil {
			return fmt.Errorf("deleting the scale set in GitHub failed; nothing else changed: %w%s", err, indent(out))
		}
		in.Out.Say("    %s", strings.TrimSpace(string(out)))
	}
	s.GitHub.Targets = slices.DeleteFunc(s.GitHub.Targets, func(t settings.Target) bool {
		return t.Key() == removed.Key()
	})
	if appGoes {
		s.GitHub.Apps = slices.DeleteFunc(s.GitHub.Apps, func(a settings.App) bool {
			return a.ClientID == removed.ClientID
		})
	}
	if err := in.writeConfig(ctx, s); err != nil {
		return err
	}
	if err := in.saveSettings(s); err != nil {
		return fmt.Errorf("the controller has the new config, but saving the settings failed; run parcon config "+
			"apply once it is fixed: %w", err)
	}
	if err := in.restartController(ctx, s); err != nil {
		return err
	}
	if appGoes {
		if err := in.removeCredential(config.CredAppKey(removed.ClientID)); err != nil {
			return err
		}
	}
	in.Out.Say("Removed %s.", removed.URL)
	return nil
}

// TargetStatus is one target, for parcon target list.
type TargetStatus struct {
	URL string `json:"url"`
	// App is the App's slug, or its Client ID if parcon didn't create it; empty until the target has one.
	App            string `json:"app,omitempty"`
	ClientID       string `json:"clientId,omitempty"`
	InstallationID int64  `json:"installationId,omitempty"`
	// SetUp reports whether the App is installed there, so the controller serves it.
	SetUp bool `json:"setUp"`
	// Sessions are its scale sets' sessions with GitHub, by scale set, when the controller reports them.
	Sessions map[string]bool `json:"sessions,omitempty"`
}

// Targets lists the organizations and repositories the runners serve. It only reads.
func (in *Installer) Targets(context.Context) ([]TargetStatus, error) {
	s, err := in.loadSettings()
	if err != nil {
		return nil, err
	}
	snap := in.readStatus()
	var out []TargetStatus
	for _, t := range s.GitHub.Targets {
		ts := TargetStatus{URL: t.URL, ClientID: t.ClientID, InstallationID: t.InstallationID, SetUp: t.Complete()}
		if app := s.GitHub.AppByID(t.ClientID); app != nil {
			ts.App = appName(*app)
		}
		if snap != nil {
			for _, ss := range snap.ScaleSets {
				if ss.TargetKey == t.Key() {
					if ts.Sessions == nil {
						ts.Sessions = map[string]bool{}
					}
					ts.Sessions[ss.Name] = ss.SessionOpen
				}
			}
		}
		out = append(out, ts)
	}
	return out, nil
}

// finishedController checks that the node has a finished install, at this parcon's release: adding or removing a
// target writes a config and unit this parcon renders, for the controller this parcon is.
func (in *Installer) finishedController(ctx context.Context) error {
	v, installed, err := in.InstalledRelease(ctx)
	if err != nil {
		return err
	}
	if !installed {
		return errors.New("parcon isn't installed on this node yet; run parcon install, which sets up the first " +
			"target")
	}
	if v != in.Version {
		return fmt.Errorf("the node is at release %s and this parcon is %s; run parcon update first", v, in.Version)
	}
	return nil
}

// targetWorkers returns the workers of a target, by its key. Workers from before several targets carry no target
// tag and belong to the first target.
func targetWorkers(vms []proxmox.VM, key string, first bool) []proxmox.VM {
	var out []proxmox.VM
	for _, vm := range vms {
		if vm.Pool != RunnerPool || !vm.HasTag(vmtags.Worker) {
			continue
		}
		tag, tagged := vmtags.String(vm, vmtags.TargetPrefix)
		if tag == key || (!tagged && first) {
			out = append(out, vm)
		}
	}
	return out
}

// appName is an App's slug, or its Client ID if parcon didn't create it.
func appName(a settings.App) string {
	if a.Slug != "" {
		return a.Slug
	}
	return a.ClientID
}

// runsOn is what a workflow puts in runs-on for the scale set.
func runsOn(s *settings.Settings) string {
	labels := s.ScaleSet.Labels
	if len(labels) == 0 {
		labels = []string{s.ScaleSet.Name}
	}
	if len(labels) == 1 {
		return labels[0]
	}
	return "[" + strings.Join(labels, ", ") + "]"
}
