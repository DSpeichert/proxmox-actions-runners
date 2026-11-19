package installer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/term"
)

// env reads what the controller's config needs from the node.
func (in *Installer) env(ctx context.Context, s *settings.Settings) (settings.Env, error) {
	node, err := in.nodeName(ctx)
	if err != nil {
		return settings.Env{}, err
	}
	addr, err := in.pveAddress(ctx, s)
	if err != nil {
		return settings.Env{}, err
	}
	tls, err := in.Sys.TLS(in.Roots)
	if err != nil {
		return settings.Env{}, err
	}
	linked, _ := in.linkedClones(ctx, s)
	return settings.Env{Node: node, PVEAddress: addr, TLS: tls, LinkedClone: linked}, nil
}

// RenderConfig renders the controller's config.yaml from the settings and the node.
func (in *Installer) RenderConfig(ctx context.Context, s *settings.Settings) ([]byte, settings.Env, error) {
	env, err := in.env(ctx, s)
	if err != nil {
		return nil, settings.Env{}, err
	}
	_, data, err := settings.ControllerConfig(s, env)
	return data, env, err
}

// pushConfig writes the controller's config, and the node's CA when the controller verifies the API against it. The
// new config goes next to the old one and replaces it only once the controller's own parcon accepts it, so a config
// its version can't read never takes its place.
func (in *Installer) pushConfig(ctx context.Context, vmid int, s *settings.Settings) error {
	data, env, err := in.RenderConfig(ctx, s)
	if err != nil {
		return err
	}
	if env.TLS.Mode == hostsys.TLSNodeCA {
		if err := in.writeControllerFile(ctx, vmid, config.CACertFile, env.TLS.CA); err != nil {
			return err
		}
	}
	next := config.DefaultPath + ".new"
	if err := in.writeControllerFile(ctx, vmid, next, data); err != nil {
		return err
	}
	in.Change.Log(fmt.Sprintf("controller VM %d: parcon check config, then mv %s %s", vmid, next, config.DefaultPath))
	if in.Change.DryRun {
		return nil
	}
	if out, err := in.asParcon(ctx, vmid, time.Minute, nil, "parcon", "check", "config", "-config", next); err != nil {
		_, _ = in.asParcon(ctx, vmid, 30*time.Second, nil, "rm", "-f", next)
		return fmt.Errorf("the controller's parcon refuses the new config: %w%s", err, indent(out))
	}
	if _, err := in.asParcon(ctx, vmid, 30*time.Second, nil, "mv", next, config.DefaultPath); err != nil {
		return fmt.Errorf("replace the controller's config: %w", err)
	}
	return nil
}

func indent(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return "\n  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// configureController writes the config, gives the controller its API token, and checks Proxmox from the VM.
func (in *Installer) configureController(ctx context.Context, vmid int, s *settings.Settings) error {
	in.Out.Step("Configure the controller")
	if err := in.pushConfig(ctx, vmid, s); err != nil {
		return err
	}
	if err := in.ensureToken(ctx, vmid); err != nil {
		return err
	}
	if err := in.grantACLs(ctx, s); err != nil {
		return err
	}
	if out, err := in.asParcon(ctx, vmid, time.Minute, nil, "parcon", "check", "proxmox"); err != nil {
		return fmt.Errorf("parcon check proxmox failed in the controller VM: %w%s", err, indent(out))
	}
	return nil
}

// ensureToken gives the controller the API token. A token's secret is shown only when it is created, so a token
// the controller already holds is kept; otherwise a new one is made and its secret piped straight into the VM,
// never logged or written to the host's disk.
func (in *Installer) ensureToken(ctx context.Context, vmid int) error {
	tokens, err := in.PVE.Tokens(ctx, PVEUser)
	if err != nil {
		tokens = nil // the user has no tokens yet, or doesn't exist in a dry run
	}
	exists := slices.Contains(tokens, TokenName)
	if exists {
		if _, err := in.asParcon(ctx, vmid, 30*time.Second, nil, "test", "-s", config.TokenSecretFile); err == nil {
			in.Out.Say("    the controller already holds token %s!%s", PVEUser, TokenName)
			return nil
		}
		if err := in.run(ctx, "pveum", "user", "token", "remove", PVEUser, TokenName); err != nil {
			return err
		}
	}
	out, err := in.Change.Run(ctx, pvecli.C("pveum", "user", "token", "add", PVEUser, TokenName, "--privsep", "1",
		"--comment", "proxmox-actions-runners controller", "--output-format", "json"))
	if err != nil || in.Change.DryRun {
		return err
	}
	var tok struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(out, &tok) != nil || tok.Value == "" {
		return errors.New("creating the API token returned no secret")
	}
	if err := in.writeControllerFile(ctx, vmid, config.TokenSecretFile, []byte(tok.Value+"\n")); err != nil {
		return err
	}
	in.Out.Say("    created token %s!%s and wrote its secret to the controller", PVEUser, TokenName)
	return nil
}

// AppMode is how install gets its GitHub App.
type AppMode string

const (
	// AppManifest creates a new App with GitHub's manifest flow, in a browser on any device.
	AppManifest AppMode = "manifest"
	// AppManual uses an existing App: its Client ID and private key.
	AppManual AppMode = "manual"
)

// setupApp creates or imports the GitHub App for the install's target, waits for the user to install it, and learns
// from the installation which organization or repository the runners serve, unless --github-url named it. Each ID is
// saved to the settings as soon as it is known, so a re-run after a failure continues with the same App instead of
// creating another.
func (in *Installer) setupApp(ctx context.Context, vmid int, s *settings.Settings, mode AppMode, clientID string) error {
	in.Out.Step("GitHub App")
	if err := in.migrateAppKey(ctx, vmid, s); err != nil {
		return err
	}
	url := ""
	if len(s.GitHub.Targets) > 0 {
		url = s.GitHub.Targets[0].URL
	}
	if t := s.GitHub.Target(url); t == nil || !t.Complete() {
		owner, _, _ := config.ParseGitHubURL(url)
		id, err := in.ensureApp(ctx, vmid, s, owner, mode, clientID)
		if err != nil {
			return err
		}
		if err := in.bindInstallation(ctx, vmid, s, id, url, "parcon install"); err != nil {
			return err
		}
	}
	return in.checkGitHub(ctx, vmid)
}

// ensureApp returns the Client ID of the App for an organization or personal account: one the settings already
// have, or a new one, created with the manifest flow or imported by hand. A new App is saved and its key pushed to
// the controller before anything else, so a re-run uses it. With no owner (install without --github-url), an install
// that already created its App reuses it.
func (in *Installer) ensureApp(ctx context.Context, vmid int, s *settings.Settings, owner string, mode AppMode,
	clientID string) (string, error) {
	if a := s.GitHub.AppFor(owner); a != nil {
		return a.ClientID, nil
	}
	if owner == "" && len(s.GitHub.Apps) == 1 && len(s.GitHub.Targets) == 0 {
		return s.GitHub.Apps[0].ClientID, nil
	}
	app := settings.App{ClientID: clientID, Owner: owner}
	if mode == AppManual {
		if clientID == "" {
			return "", errors.New("--app manual needs --client-id, the existing App's Client ID")
		}
		if err := in.importAppKey(ctx, vmid, clientID); err != nil {
			return "", err
		}
	} else {
		created, err := in.createApp(ctx, vmid)
		if err != nil {
			return "", err
		}
		app = settings.App{ClientID: created.ClientID, Slug: created.Slug, Owner: created.Owner}
		if owner != "" && !strings.EqualFold(created.Owner, owner) {
			return "", in.wrongOwner(ctx, vmid, s, app, owner)
		}
	}
	s.GitHub.Apps = append(s.GitHub.Apps, app)
	if err := s.Validate(); err != nil {
		return "", err
	}
	return app.ClientID, in.saveAndPush(ctx, vmid, s)
}

// wrongOwner handles an App the manifest flow created for another account than the target's, because the user chose
// that account on the helper page. If that account has no App yet, the new one is kept for it; otherwise its key is
// removed, since the account already has an App.
func (in *Installer) wrongOwner(ctx context.Context, vmid int, s *settings.Settings, app settings.App,
	wanted string) error {
	if other := s.GitHub.AppFor(app.Owner); other != nil {
		in.Change.Log(fmt.Sprintf("controller VM %d: remove the new App's key, %s", vmid,
			config.AppKeyFile(app.ClientID)))
		if _, err := in.asParcon(ctx, vmid, guestTimeout, nil, "rm", "-f", config.AppKeyFile(app.ClientID)); err != nil {
			return err
		}
		return fmt.Errorf("the new App %s belongs to %s, which already has App %s, not to %s; delete %s on GitHub "+
			"(https://github.com/apps/%s, then Advanced) and run this again, choosing %s on the page", app.Slug,
			app.Owner, other.ClientID, wanted, app.Slug, app.Slug, wanted)
	}
	s.GitHub.Apps = append(s.GitHub.Apps, app)
	if err := in.saveAndPush(ctx, vmid, s); err != nil {
		return err
	}
	return fmt.Errorf("the new App %s belongs to %s, not to %s; parcon keeps it for %s (parcon target add "+
		"--github-url https://github.com/%s uses it). Run this again and choose %s on the page", app.Slug, app.Owner,
		wanted, app.Owner, app.Owner, wanted)
}

// bindInstallation waits for the App to be installed for a target and saves where: the target's App and
// installation. url names the target; empty means the organization or repository the user installs the App on, as in
// an install without --github-url. A repository target also waits until the installation reaches the repository.
// again names the command that keeps waiting if the wait times out.
func (in *Installer) bindInstallation(ctx context.Context, vmid int, s *settings.Settings, clientID, url,
	again string) error {
	app := s.GitHub.AppByID(clientID)
	account, repo, _ := config.ParseGitHubURL(url)
	if account == "" {
		account = app.Owner
	}
	in.askToInstall(*app, url)
	args := []string{"parcon", "github", "app", "wait-installation", "-client-id", clientID}
	if account != "" {
		args = append(args, "-account", account)
	}
	if repo != "" {
		args = append(args, "-repository", repo)
	}
	out, err := in.asParcon(ctx, vmid, 16*time.Minute, nil, args...)
	if err != nil {
		return fmt.Errorf("the App wasn't installed; run %s again to keep waiting: %w", again, err)
	}
	var inst installation
	if err := json.Unmarshal(out, &inst); err != nil || inst.InstallationID == 0 {
		return fmt.Errorf("parcon github app wait-installation printed %q", out)
	}
	target, err := installationTarget(inst, url, in.Term)
	if err != nil {
		return err
	}
	if url != "" && config.TargetKey(target) != config.TargetKey(url) {
		return fmt.Errorf("the App is installed for %s, but the target is %s", target, url)
	}
	t := s.GitHub.Target(target)
	if t == nil {
		s.GitHub.Targets = append(s.GitHub.Targets, settings.Target{URL: target})
		t = &s.GitHub.Targets[len(s.GitHub.Targets)-1]
	}
	t.ClientID, t.InstallationID = clientID, inst.InstallationID
	if app := s.GitHub.AppByID(clientID); app.Owner == "" {
		app.Owner = inst.Account
	}
	if err := s.Validate(); err != nil {
		return err
	}
	in.Out.Say("    the App is installed; the runners serve %s", t.URL)
	return in.saveAndPush(ctx, vmid, s)
}

// checkGitHub runs parcon check github in the controller VM, which checks every target's App.
func (in *Installer) checkGitHub(ctx context.Context, vmid int) error {
	if out, err := in.asParcon(ctx, vmid, time.Minute, nil, "parcon", "check", "github"); err != nil {
		return fmt.Errorf("parcon check github failed in the controller VM; if an App was deleted in GitHub, "+
			"remove its targets with parcon target remove and add them again: %w%s", err, indent(out))
	}
	return nil
}

// migrateAppKey copies the one App's key from where releases before several Apps kept it to its own path, when the
// settings were just migrated from that format. It copies rather than moves, so the controller keeps a key it can
// read whatever happens next; parcon update removes the old file once the controller runs with the new config.
func (in *Installer) migrateAppKey(ctx context.Context, vmid int, s *settings.Settings) error {
	if s.MigratedFrom() == 0 || len(s.GitHub.Apps) == 0 {
		return nil
	}
	dst := config.AppKeyFile(s.GitHub.Apps[0].ClientID)
	in.Change.Log(fmt.Sprintf("controller VM %d: copy the GitHub App key %s to %s", vmid, config.LegacyAppKeyFile,
		dst))
	if in.Change.DryRun {
		return nil
	}
	_, err := in.asParcon(ctx, vmid, guestTimeout, nil, "sh", "-c", `test -e "$2" || ! test -e "$1" || cp -p "$1" "$2"`,
		"sh", config.LegacyAppKeyFile, dst)
	if err != nil {
		return fmt.Errorf("copying the GitHub App key failed: %w", err)
	}
	return nil
}

// saveAndPush saves the settings and pushes the controller's config.
func (in *Installer) saveAndPush(ctx context.Context, vmid int, s *settings.Settings) error {
	if err := in.saveSettings(s); err != nil {
		return err
	}
	return in.pushConfig(ctx, vmid, s)
}

// installation is what `parcon github app wait-installation` prints.
type installation struct {
	InstallationID int64    `json:"installationId"`
	Account        string   `json:"account"`
	AccountType    string   `json:"accountType"`
	Repositories   []string `json:"repositories"`
}

// installationTarget returns the organization or repository the runners serve. An organization's runners serve the
// organization. A personal account can only have repository runners: with one repository, that's the one; with
// several, --github-url or the user picks.
func installationTarget(inst installation, wanted string, t term.Terminal) (string, error) {
	base := "https://github.com/" + inst.Account
	if inst.AccountType == "Organization" {
		return base, nil
	}
	switch len(inst.Repositories) {
	case 0:
		return "", fmt.Errorf("the App is installed on %s's personal account without a repository; add one under "+
			"the App's installation settings and run parcon install again", inst.Account)
	case 1:
		return base + "/" + inst.Repositories[0], nil
	}
	for _, r := range inst.Repositories {
		if wanted == base+"/"+r {
			return wanted, nil
		}
	}
	i, err := t.Choose(fmt.Sprintf("    The App can reach several of %s's repositories. Which one do the runners "+
		"serve?", inst.Account), inst.Repositories)
	if errors.Is(err, term.ErrNoTerminal) {
		return "", fmt.Errorf("the App can reach several of %s's repositories (%s); run parcon install again with "+
			"--github-url %s/<repo> for the one the runners serve", inst.Account,
			strings.Join(inst.Repositories, " "), base)
	}
	if err != nil {
		return "", err
	}
	return base + "/" + inst.Repositories[i], nil
}

// newState returns a random state for the helper page's link: 32 bytes from the kernel's cryptographic random
// source, in unpadded base64url, so nobody can guess the line install will accept.
func newState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on Linux.
	return base64.RawURLEncoding.EncodeToString(b)
}

// askToInstall tells the user to install the App for a target, url, or wherever they choose if url is empty, while
// parcon waits for it. The install link is the very last line, so it is what the user sees while the terminal waits.
func (in *Installer) askToInstall(app settings.App, url string) {
	_, repo, _ := config.ParseGitHubURL(url)
	switch {
	case repo != "":
		in.Out.Say(`
    Now install the App for the repository %s. If it is already installed on the account, add the repository
    to that installation (Configure, then Repository access). parcon waits up to 15 minutes, and carries on by
    itself once it is installed.`, url)
	case url != "":
		in.Out.Say(`
    Now install the App on %s, for all repositories. parcon waits up to 15 minutes, and carries on by itself once
    it is installed.`, url)
	default:
		in.Out.Say(`
    Now install the App. On an organization, install it for all repositories; on your personal account, choose
    the repository the runners serve. parcon waits up to 15 minutes, and carries on by itself once it is installed.`)
	}
	if app.Slug == "" {
		// An existing App: parcon doesn't know its name, so it can't make the link.
		in.Out.Say(`
    Install App %s from its settings on GitHub (Settings > Developer settings > GitHub Apps > the App > Install
    App), unless it is installed already.`, app.ClientID)
		return
	}
	in.Out.Say(`
    Open this link to install it:

      https://github.com/apps/%s/installations/new`, app.Slug)
}

// createdApp is what `parcon github app create` prints.
type createdApp struct {
	ClientID string `json:"clientId"`
	Slug     string `json:"slug"`
	Owner    string `json:"owner"`
}

// createApp runs the manifest flow and returns the new App. The code the user pastes goes only into the controller
// VM, on stdin, and the App's key stays there.
func (in *Installer) createApp(ctx context.Context, vmid int) (createdApp, error) {
	state := newState()
	in.Out.Say(`
    Create your GitHub App in a browser on any device:

      %s?state=%s

    Choose your organization or your personal account there, then create the App on GitHub. The page then shows
    one line to copy. Paste it here.
`, HelperURL, state)
	line, err := in.Term.Line("    Line from the page: ")
	if errors.Is(err, term.ErrNoTerminal) {
		return createdApp{}, errors.New("creating the GitHub App needs a terminal; run parcon install in one, or use an " +
			"existing App with --app manual --client-id")
	}
	if err != nil {
		return createdApp{}, err
	}
	got, code, ok := strings.Cut(strings.TrimSpace(line), ".")
	if !ok || got != state || code == "" {
		return createdApp{}, errors.New("that line isn't from this install's link; run parcon install again")
	}
	out, err := in.asParcon(ctx, vmid, time.Minute, []byte(code+"\n"), "parcon", "github", "app", "create")
	if err != nil {
		return createdApp{}, fmt.Errorf("creating the GitHub App failed: %w", err)
	}
	var app createdApp
	if err := json.Unmarshal(out, &app); err != nil || app.ClientID == "" {
		return createdApp{}, fmt.Errorf("parcon github app create printed %q", out)
	}
	in.Out.Say("\n    Created GitHub App %s, owned by %s.", app.Slug, app.Owner)
	return app, nil
}

// importAppKey reads an existing App's private key from the terminal and pipes it into the controller VM.
func (in *Installer) importAppKey(ctx context.Context, vmid int, clientID string) error {
	key, err := in.Term.PEM("    Paste the App's private key (the whole PEM, ending with its END line):")
	if errors.Is(err, term.ErrNoTerminal) {
		return errors.New("--app manual reads the App's private key from the terminal; run parcon install in one")
	}
	if err != nil {
		return err
	}
	if _, err := in.asParcon(ctx, vmid, 30*time.Second, []byte(key), "parcon", "github", "app", "import",
		"-client-id", clientID); err != nil {
		return fmt.Errorf("importing the key failed: %w", err)
	}
	return nil
}

// startController enables and starts parcon.service, and returns when it started.
func (in *Installer) startController(ctx context.Context, vmid int) (time.Time, error) {
	in.Out.Step("Start the controller")
	started := in.Now()
	in.Change.Log(fmt.Sprintf("controller VM %d: systemctl enable --now parcon.service", vmid))
	if _, err := in.guestExec(ctx, vmid, time.Minute, nil, "systemctl", "enable", "--now", "parcon.service"); err != nil {
		return started, fmt.Errorf("starting parcon.service failed: %w", err)
	}
	return started, nil
}

// waitSessions waits for the controller to log that its scale set sessions are open for every target the settings
// complete, which it does once each scale set is registered and listening. Each such line names its target, since
// the controller logs JSON with a target attribute.
func (in *Installer) waitSessions(ctx context.Context, vmid int, s *settings.Settings, since time.Time,
	within time.Duration) error {
	want := map[string]bool{}
	for _, t := range s.GitHub.CompleteTargets() {
		want[t.URL] = true
	}
	for deadline := in.Now().Add(within); ; {
		out, err := in.guestExec(ctx, vmid, 30*time.Second, nil, "journalctl", "-u", "parcon.service", "--since",
			"@"+strconv.FormatInt(since.Unix(), 10), "-o", "cat")
		missing := slices.Sorted(maps.Keys(want))
		if err == nil {
			missing = slices.DeleteFunc(missing, sessionOpened(out))
			if len(missing) == 0 {
				return nil
			}
		}
		if in.Now().After(deadline) {
			return fmt.Errorf("the controller didn't open its scale set sessions for %s within %s; see parcon status "+
				"and journalctl -u parcon in VM %d", strings.Join(missing, ", "), within, vmid)
		}
		if err := in.Sleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

// sessionOpened returns whether the controller's log says a session for a target opened.
func sessionOpened(log []byte) func(target string) bool {
	opened := map[string]bool{}
	for line := range strings.Lines(string(log)) {
		var entry struct {
			Msg    string `json:"msg"`
			Target string `json:"target"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "scale set session opened" {
			opened[entry.Target] = true
		}
	}
	return func(target string) bool { return opened[target] }
}
