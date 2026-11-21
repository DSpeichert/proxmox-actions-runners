package installer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// addTarget adds a target to an installed node, answering the manifest flow with code.
func addTarget(t *testing.T, ti *testInstaller, url, code string) error {
	t.Helper()
	ti.Term = &linkAnswerer{ti: ti, code: code}
	ti.Yes = true
	return ti.AddTarget(context.Background(), TargetOptions{URL: url, App: AppManifest})
}

func loadGitHub(t *testing.T, ti *testInstaller) *settings.GitHub {
	t.Helper()
	s, err := settings.Load(ti.SettingsPath)
	must(t, err)
	return &s.GitHub
}

// pushedConfig is the controller's config, as the installer last wrote it.
func pushedConfig(t *testing.T, ti *testInstaller) *config.Config {
	t.Helper()
	return ti.controllerConfig(t)
}

// hasCredential reports whether the controller has a credential.
func hasCredential(ti *testInstaller, name string) bool {
	_, err := os.Stat(filepath.Join(ti.CredStoreDir, name))
	return err == nil
}

func TestAddTargetOfAnotherAccount(t *testing.T) {
	ti := installed(t)
	err := addTarget(t, ti, "https://github.com/octocat/hello/", userManifestCode)
	if err != nil {
		t.Fatalf("AddTarget: %v\n%s", err, ti.stdout)
	}
	ti.assertNoSecrets(t, err)

	want := settings.GitHub{
		Apps: []settings.App{{ClientID: "Iv23liEXAMPLE", Slug: "my-runners", Owner: "my-org"},
			{ClientID: "Iv23liUSER", Slug: "octocat-runners", Owner: "octocat"}},
		Targets: []settings.Target{{URL: "https://github.com/my-org", ClientID: "Iv23liEXAMPLE", InstallationID: 7},
			{URL: "https://github.com/octocat/hello", ClientID: "Iv23liUSER", InstallationID: 9}},
	}
	if got := loadGitHub(t, ti); !reflect.DeepEqual(*got, want) {
		t.Errorf("settings github = %+v\nwant %+v", got, want)
	}
	cfg := pushedConfig(t, ti)
	if len(cfg.GitHub.Targets) != 2 || cfg.GitHub.Apps[1].PrivateKeyFile != config.CredentialPath(config.CredAppKey("Iv23liUSER")) {
		t.Errorf("pushed config github = %+v", cfg.GitHub)
	}
	out := ti.stdout.String()
	for _, w := range []string{"GitHub App: a new one for octocat", "for the repository",
		"Workers: at most 1 at once (runners.max), shared by all 2 targets",
		"Now install the App for the repository https://github.com/octocat/hello",
		"runs-on: proxmox-ubuntu-26.04"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	// The installation is bound to the account and the repository, and the controller restarted with it.
	if !slices.ContainsFunc(ti.node.lines(), func(l string) bool {
		return strings.HasPrefix(l, "systemd-run ") && strings.HasSuffix(l, " github app wait-installation "+
			"-client-id Iv23liUSER -account octocat -repository hello")
	}) {
		t.Errorf("wait-installation:\n%s", strings.Join(ti.node.lines(), "\n"))
	}
	if !ti.node.ran("systemctl try-restart parcon.service") {
		t.Error("the controller wasn't restarted")
	}
}

func TestAddTargetReusesTheAccountsApp(t *testing.T) {
	ti := installed(t)
	must(t, addTarget(t, ti, "https://github.com/octocat/hello", userManifestCode))
	ti.node.calls = nil
	ti.stdout.Reset()

	must(t, addTarget(t, ti, "https://github.com/Octocat/world", ""))
	if strings.Contains(ti.stdout.String(), "Created GitHub App") {
		t.Error("created another App for the same account")
	}
	if !strings.Contains(ti.stdout.String(), "GitHub App: octocat-runners, which already serves "+
		"https://github.com/octocat/hello") {
		t.Errorf("plan:\n%s", ti.stdout)
	}
	g := loadGitHub(t, ti)
	if len(g.Apps) != 2 || len(g.Targets) != 3 || g.Targets[2] != (settings.Target{URL: "https://github.com/Octocat/world",
		ClientID: "Iv23liUSER", InstallationID: 9}) {
		t.Errorf("settings github = %+v", g)
	}
}

func TestAddTargetRefusesOneItHas(t *testing.T) {
	ti := installed(t)
	err := addTarget(t, ti, "https://github.com/My-Org", "")
	if err == nil || !strings.Contains(err.Error(), "is already a target") {
		t.Errorf("AddTarget = %v", err)
	}
}

func TestAddTargetContinuesARepositoryNotInTheInstallationYet(t *testing.T) {
	ti := installed(t)
	must(t, addTarget(t, ti, "https://github.com/octocat/hello", userManifestCode))
	err := addTarget(t, ti, "https://github.com/octocat/later", "")
	if err == nil || !strings.Contains(err.Error(), "run parcon target add --github-url "+
		"https://github.com/octocat/later again") {
		t.Fatalf("AddTarget = %v", err)
	}
	// The target waits in the settings, and the controller doesn't serve it yet.
	g := loadGitHub(t, ti)
	if tg := g.Target("https://github.com/octocat/later"); tg == nil || tg.Complete() {
		t.Errorf("targets = %+v", g.Targets)
	}
	if n := len(pushedConfig(t, ti).GitHub.Targets); n != 2 {
		t.Errorf("the controller's config has %d targets, want the 2 set up", n)
	}

	// Once the user adds the repository on GitHub, a re-run finishes it.
	inst := ti.node.installations["octocat"]
	inst.Repositories = append(inst.Repositories, "later")
	ti.node.installations["octocat"] = inst
	must(t, addTarget(t, ti, "https://github.com/octocat/later", ""))
	if tg := loadGitHub(t, ti).Target("https://github.com/octocat/later"); tg == nil || !tg.Complete() {
		t.Errorf("target = %+v", tg)
	}
}

func TestAddTargetWithAnAppOfAnotherAccount(t *testing.T) {
	ti := installed(t)
	// The user picked their personal account on the helper page, not other-org: the App is kept for octocat.
	err := addTarget(t, ti, "https://github.com/other-org", userManifestCode)
	if err == nil || !strings.Contains(err.Error(), "belongs to octocat, not to other-org; parcon keeps it") {
		t.Fatalf("AddTarget = %v", err)
	}
	if a := loadGitHub(t, ti).AppFor("octocat"); a == nil || a.ClientID != "Iv23liUSER" {
		t.Errorf("the App wasn't kept for octocat: %+v", loadGitHub(t, ti).Apps)
	}
	// The same again: octocat has an App now, so the new one's key is removed.
	ti.node.apps["SENTINEL-third-code"] = fakeApp{ClientID: "Iv23liTHIRD", Slug: "third", Owner: "octocat"}
	ti.Term = &linkAnswerer{ti: ti, code: "SENTINEL-third-code"}
	err = ti.AddTarget(context.Background(), TargetOptions{URL: "https://github.com/other-org", App: AppManifest})
	if err == nil || !strings.Contains(err.Error(), "which already has App Iv23liUSER") {
		t.Fatalf("AddTarget = %v", err)
	}
	if hasCredential(ti, config.CredAppKey("Iv23liTHIRD")) {
		t.Error("the extra App's key is still a credential")
	}
	if g := loadGitHub(t, ti); len(g.Apps) != 2 {
		t.Errorf("apps = %+v", g.Apps)
	}
}

func TestAddTargetWithAnExistingApp(t *testing.T) {
	ti := installed(t)
	ti.Term = &linkAnswerer{ti: ti}
	ti.Yes = true
	err := ti.AddTarget(context.Background(), TargetOptions{URL: "https://github.com/octocat/hello", App: AppManual,
		ClientID: "Iv23liEXISTING"})
	if err != nil {
		t.Fatal(err)
	}
	ti.assertNoSecrets(t, err)
	if key := ti.node.credentials[config.CredAppKey("Iv23liEXISTING")]; key != appKeyPEM {
		t.Errorf("key encrypted for the controller = %q", key)
	}
}

func TestAddTargetDryRunChangesNothing(t *testing.T) {
	ti := installed(t)
	before, _ := os.ReadFile(ti.SettingsPath)
	ti.Change.DryRun = true
	must(t, addTarget(t, ti, "https://github.com/octocat/hello", userManifestCode))
	if after, _ := os.ReadFile(ti.SettingsPath); string(after) != string(before) {
		t.Error("the settings changed")
	}
	for _, l := range ti.node.lines() {
		if strings.Contains(l, "systemd-") || strings.Contains(l, "try-restart") {
			t.Errorf("a dry run ran %s", l)
		}
	}
	if !strings.Contains(ti.stdout.String(), "Plan") {
		t.Errorf("no plan:\n%s", ti.stdout)
	}
}

func TestRemoveTarget(t *testing.T) {
	ti := installed(t)
	must(t, addTarget(t, ti, "https://github.com/octocat/hello", userManifestCode))
	must(t, addTarget(t, ti, "https://github.com/octocat/world", ""))
	ti.node.calls = nil

	// hello shares its App with world, so the App's key stays.
	must(t, ti.RemoveTarget(context.Background(), "https://github.com/octocat/hello"))
	ti.assertNoSecrets(t)
	g := loadGitHub(t, ti)
	if len(g.Targets) != 2 || g.Target("https://github.com/octocat/hello") != nil || len(g.Apps) != 2 {
		t.Errorf("settings github = %+v", g)
	}
	// The scale sets go first, with the config that still names the target; then the config without it.
	lines := ti.node.lines()
	del := slices.IndexFunc(lines, func(l string) bool {
		return strings.HasSuffix(l, "parcon github scaleset delete -target https://github.com/octocat/hello")
	})
	restart := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "try-restart") })
	if del < 0 || restart < del {
		t.Errorf("scale set deleted at %d, controller restarted at %d:\n%s", del, restart, strings.Join(lines, "\n"))
	}
	if !hasCredential(ti, config.CredAppKey("Iv23liUSER")) {
		t.Error("the App's key was removed while world still uses it")
	}

	// world is the App's last target, so its key goes too.
	must(t, ti.RemoveTarget(context.Background(), "https://github.com/octocat/world"))
	if hasCredential(ti, config.CredAppKey("Iv23liUSER")) {
		t.Error("the unused App's key is still a credential")
	}
	if g := loadGitHub(t, ti); len(g.Apps) != 1 || len(g.Targets) != 1 {
		t.Errorf("settings github = %+v", g)
	}

	// The last target goes only with the install.
	err := ti.RemoveTarget(context.Background(), "https://github.com/my-org")
	if err == nil || !strings.Contains(err.Error(), "parcon uninstall") {
		t.Errorf("RemoveTarget of the last target = %v", err)
	}
	if err := ti.RemoveTarget(context.Background(), "https://github.com/nobody"); err == nil {
		t.Error("removed a target that isn't one")
	}
}

func TestRemoveTargetStopsWhenGitHubRefuses(t *testing.T) {
	ti := installed(t)
	must(t, addTarget(t, ti, "https://github.com/octocat/hello", userManifestCode))
	before, _ := os.ReadFile(ti.SettingsPath)
	ti.node.controller = func(argv []string) (int, string, string, bool) {
		if strings.HasPrefix(strings.Join(argv, " "), "github scaleset delete") {
			return 1, "", "GitHub refuses", true
		}
		return 0, "", "", false
	}
	err := ti.RemoveTarget(context.Background(), "https://github.com/octocat/hello")
	if err == nil || !strings.Contains(err.Error(), "nothing else changed") {
		t.Fatalf("RemoveTarget = %v", err)
	}
	if after, _ := os.ReadFile(ti.SettingsPath); string(after) != string(before) {
		t.Error("the settings changed")
	}
}

func TestTargets(t *testing.T) {
	ti := installed(t)
	must(t, addTarget(t, ti, "https://github.com/octocat/hello", userManifestCode))
	_ = addTarget(t, ti, "https://github.com/octocat/later", "") // waits for the repository
	targets, err := ti.Targets(context.Background())
	must(t, err)
	if len(targets) != 3 || !targets[0].SetUp || targets[1].App != "octocat-runners" || targets[2].SetUp {
		t.Errorf("targets = %+v", targets)
	}
}
