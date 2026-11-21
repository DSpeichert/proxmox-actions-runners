package installer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"maps"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

var defaultLookPath = exec.LookPath

// writeNodeCert writes a node CA and the node's certificate, as Proxmox makes them.
func writeNodeCert(t *testing.T, pveDir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Proxmox Virtual Environment"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	must(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "pve1"},
		DNSNames: []string{"localhost", "pve1"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caCert, _ := x509.ParseCertificate(caDER)
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, key)
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(pveDir, "local"), 0o755))
	must(t, os.WriteFile(filepath.Join(pveDir, "pve-root-ca.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644))
	must(t, os.WriteFile(filepath.Join(pveDir, "local/pve-ssl.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o644))
}

func defaultOptions() InstallOptions {
	s := settings.Default()
	return InstallOptions{Settings: &s, App: AppManifest}
}

// answerManifest answers the manifest flow's question with the line for the link install prints. The state in the
// link is random, so the answer is read from the output.
func (ti *testInstaller) answerManifest() { ti.Term = &linkAnswerer{ti: ti} }

// linkAnswerer answers the manifest flow's question with the state from the link install printed.
type linkAnswerer struct {
	ti      *testInstaller
	prompts []string
	// code is the manifest code the page shows; empty means manifestCode.
	code string
}

var stateInLink = regexp.MustCompile(`\?state=([A-Za-z0-9_-]+)`)

func (l *linkAnswerer) Confirm(q string) (bool, error) {
	l.prompts = append(l.prompts, q)
	return true, nil
}
func (l *linkAnswerer) Line(p string) (string, error) {
	l.prompts = append(l.prompts, p)
	all := stateInLink.FindAllStringSubmatch(l.ti.stdout.String(), -1)
	var m []string
	for _, a := range all {
		m = append(m, a[1])
	}
	if m == nil {
		return "", errors.New("no link printed")
	}
	code := l.code
	if code == "" {
		code = manifestCode
	}
	// The newest link is the one the question is about.
	return m[len(m)-1] + "." + code, nil
}
func (l *linkAnswerer) PEM(p string) (string, error) {
	l.prompts = append(l.prompts, p)
	return appKeyPEM, nil
}
func (l *linkAnswerer) Choose(p string, _ []string) (int, error) {
	l.prompts = append(l.prompts, p)
	return 0, nil
}

// assertNoSecrets checks that no secret reached a command line, the output, or the host's files.
func (ti *testInstaller) assertNoSecrets(t *testing.T, errs ...error) {
	t.Helper()
	secrets := []string{tokenSecret, manifestCode, userManifestCode, appKeyLine}
	for _, l := range ti.node.lines() {
		for _, s := range secrets {
			if strings.Contains(l, s) {
				t.Errorf("a command line holds a secret: %s", l)
			}
		}
	}
	for _, text := range []string{ti.stdout.String(), ti.stderr.String()} {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("the output holds a secret:\n%s", text)
			}
		}
	}
	for _, err := range errs {
		for _, s := range secrets {
			if err != nil && strings.Contains(err.Error(), s) {
				t.Errorf("an error holds a secret: %v", err)
			}
		}
	}
	_ = filepath.WalkDir(ti.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, _ := os.ReadFile(path)
		for _, s := range secrets {
			if strings.Contains(string(data), s) {
				t.Errorf("%s on the host holds a secret", path)
			}
		}
		return nil
	})
}

// controllerConfig returns the controller's config.
func (ti *testInstaller) controllerConfig(t *testing.T) *config.Config {
	t.Helper()
	data, err := os.ReadFile(ti.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("controller config: %v\n%s", err, data)
	}
	return cfg
}

func TestInstall(t *testing.T) {
	ti := newTestInstaller(t)
	ti.answerManifest()
	ctx := context.Background()
	err := ti.Install(ctx, defaultOptions())
	if err != nil {
		t.Fatalf("Install: %v\n%s\n%s", err, ti.stdout, ti.stderr)
	}
	ti.assertNoSecrets(t, err)

	// parcon and its settings are on the host, with what the install learned.
	if data, _ := os.ReadFile(ti.BinaryPath); string(data) != "parcon 0.2.0" {
		t.Errorf("host binary = %q", data)
	}
	s, err := settings.Load(ti.SettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	wantGitHub := settings.GitHub{
		Apps:    []settings.App{{ClientID: "Iv23liEXAMPLE", Slug: "my-runners", Owner: "my-org"}},
		Targets: []settings.Target{{URL: "https://github.com/my-org", ClientID: "Iv23liEXAMPLE", InstallationID: 7}},
	}
	if !reflect.DeepEqual(s.GitHub, wantGitHub) || s.Version != settings.Version || s.Installed != "0.2.0" {
		t.Errorf("settings github = %+v, want %+v; installed %q", s.GitHub, wantGitHub, s.Installed)
	}
	// The installation is looked up on the account that owns the new App, never taken on trust.
	if !slices.ContainsFunc(ti.node.lines(), func(l string) bool {
		return strings.Contains(l, "wait-installation -client-id Iv23liEXAMPLE -account my-org")
	}) {
		t.Errorf("wait-installation wasn't bound to the App's owner:\n%s", strings.Join(ti.node.lines(), "\n"))
	}

	// Proxmox objects.
	n := ti.node
	if _, ok := n.pools[SystemPool]; ok || n.pools[RunnerPool] != PoolComment {
		t.Errorf("pools = %v", n.pools)
	}
	if !strings.Contains(n.roles[Role], "VM.GuestAgent.FileWrite") || strings.Contains(n.roles[Role], "Unrestricted") {
		t.Errorf("role privileges = %s", n.roles[Role])
	}
	if !slices.Contains(n.tokens, TokenName) || len(n.acls) != 6 {
		t.Errorf("tokens %v, acls %v", n.tokens, n.acls)
	}

	// The worker network: SDN DHCP, and a subnet the host routes and SNATs, with the host's DNS server.
	if o := n.zoneOpts[Zone]; o["dhcp"] != "dnsmasq" || o["ipam"] != "pve" {
		t.Errorf("zone options = %v", o)
	}
	wantSubnet := pvecli.Subnet{ID: "parzone-10.251.0.0-22", CIDR: "10.251.0.0/22", Gateway: "10.251.0.1", SNAT: 1,
		DHCPRange: []pvecli.DHCPRange{{Start: "10.251.0.2", End: "10.251.3.254"}}, DNSServer: "192.0.2.1"}
	if len(n.subnets) != 1 || !reflect.DeepEqual(n.subnets[0], wantSubnet) {
		t.Errorf("subnets = %+v", n.subnets)
	}

	// The worker firewall: what workers must not reach, including the LAN and the host the controller runs on,
	// and the group that lets DNS through to the host's resolver and drops the rest of it.
	for _, want := range []string{"10.0.0.0/8", "192.168.0.0/16", "192.0.2.0/24", "192.0.2.5"} {
		if !slices.Contains(n.ipsets[BlockedIPSet], want) {
			t.Errorf("IP set %s = %v, lacks %s", BlockedIPSet, n.ipsets[BlockedIPSet], want)
		}
	}
	// The worker subnet is SDN's own IP set, not a copy.
	if slices.Contains(n.ipsets[BlockedIPSet], "10.251.0.0/22") {
		t.Errorf("IP set %s = %v, copies the worker subnet", BlockedIPSet, n.ipsets[BlockedIPSet])
	}
	if !sameRules(n.groups[FirewallGroup], workerRules(netipMust("192.0.2.1"))) {
		t.Errorf("group rules = %+v", n.groups[FirewallGroup])
	}
	if len(n.hostRules) != 1 || !sameRule(n.hostRules[0], hostRule()) {
		t.Errorf("host rules = %+v", n.hostRules)
	}

	// VMs: a template with the worker firewall in the reserved IDs, and no smoke-test clone left.
	tmpl := n.vmsTagged(vmtags.Template)
	if len(tmpl) != 1 || tmpl[0] != 10096 || !n.vms[10096].template ||
		!slices.Contains(n.vms[10096].tags, "par-rv-2.338.0") {
		t.Errorf("templates %v", tmpl)
	}
	tv := n.vms[10096]
	if tv.config["net0"] != "virtio,bridge=parnet,firewall=1" || tv.fwOptions["enable"] != "1" ||
		tv.fwOptions["policy_in"] != "DROP" || tv.fwOptions["dhcp"] != "1" || len(tv.fwRules) != 1 ||
		tv.fwRules[0].Type != "group" || tv.fwRules[0].Action != FirewallGroup {
		t.Errorf("template firewall: net0 %q, options %v, rules %+v", tv.config["net0"], tv.fwOptions, tv.fwRules)
	}
	if len(n.vmsTagged(vmtags.Build)) != 0 {
		t.Error("the smoke-test clone is left")
	}
	if len(n.vms) != 1 {
		t.Errorf("VMs: %v, want only the template", slices.Collect(maps.Keys(n.vms)))
	}

	// The controller: its config with the App, the CA, its unit, its secrets encrypted, and its service on.
	cfg := ti.controllerConfig(t)
	if err := cfg.RequireGitHub(); err != nil || cfg.Proxmox.URL != "https://192.0.2.5:8006/api2/json" ||
		cfg.Proxmox.TokenSecretFile != "${CREDENTIALS_DIRECTORY}/parcon.pve-token" ||
		cfg.GitHub.Apps[0].PrivateKeyFile != "${CREDENTIALS_DIRECTORY}/parcon.github-app-Iv23liEXAMPLE" {
		t.Errorf("controller config: %v, %+v, %+v", err, cfg.Proxmox, cfg.GitHub)
	}
	if ca, err := os.ReadFile(ti.CACertPath); err != nil || len(ca) == 0 {
		t.Errorf("CA: %v", err)
	}
	if n.credentials["parcon.pve-token"] != tokenSecret || n.credentials["parcon.github-app-Iv23liEXAMPLE"] != appKeyPEM {
		t.Errorf("credentials: %v", slices.Collect(maps.Keys(n.credentials)))
	}
	for _, name := range []string{"parcon.pve-token", "parcon.github-app-Iv23liEXAMPLE"} {
		if st, err := os.Stat(filepath.Join(ti.CredStoreDir, name)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("credential %s: %v", name, err)
		}
	}
	unit, err := os.ReadFile(ti.UnitPath)
	if err != nil || !strings.Contains(string(unit), "DynamicUser=yes\n") ||
		!strings.Contains(string(unit), "LoadCredential=parcon.config:"+ti.ConfigPath+"\n") ||
		!strings.Contains(string(unit), "LoadCredential=parcon.pve-ca.pem:"+ti.CACertPath+"\n") ||
		!strings.Contains(string(unit), "ImportCredential=parcon.*\n") {
		t.Errorf("unit: %v\n%s", err, unit)
	}
	if !n.services[ServiceName] || !n.ran("systemctl daemon-reload") {
		t.Error("parcon.service isn't enabled")
	}
	// Proxmox is checked as the controller, in its sandbox.
	if !slices.ContainsFunc(n.lines(), func(l string) bool {
		return strings.HasPrefix(l, "systemd-run ") && strings.Contains(l, "--property=DynamicUser=yes") &&
			strings.HasSuffix(l, ti.BinaryPath+" check proxmox")
	}) {
		t.Errorf("parcon check proxmox didn't run as the controller:\n%s", strings.Join(n.lines(), "\n"))
	}
	if !strings.Contains(ti.stdout.String(), "runs-on: proxmox-ubuntu-26.04") {
		t.Errorf("summary:\n%s", ti.stdout)
	}

	// A second install refuses: the node has a finished install.
	if err := ti.Install(ctx, defaultOptions()); !errors.Is(err, ErrInstalled) {
		t.Errorf("second Install = %v", err)
	}
}

func TestInstallDryRunChangesNothing(t *testing.T) {
	ti := newTestInstaller(t)
	ti.Change.DryRun = true
	if err := ti.Install(context.Background(), defaultOptions()); err != nil {
		t.Fatal(err)
	}
	for _, l := range ti.node.lines() {
		if regexp.MustCompile(`^(pveum \S+ (add|modify|delete)|qm (create|set|start|destroy)|pvesh (create|set))`).
			MatchString(l) {
			t.Errorf("dry run ran %s", l)
		}
	}
	if _, err := os.Stat(ti.SettingsPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("dry run wrote the settings")
	}
	out := ti.stdout.String()
	for _, want := range []string{"==> Plan", "install parcon as " + ti.BinaryPath,
		"subnet 10.251.0.0/22: the host at 10.251.0.1 with SNAT", "DNS 192.0.2.1", "security group par-worker",
		"at most 1, each 2 vCPU and 8GiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
}

func TestInstallContinuesWithTheSameApp(t *testing.T) {
	ti := newTestInstaller(t)
	ti.answerManifest()
	ctx := context.Background()
	// The first run stops while waiting for the App's installation.
	ti.node.controller = func(argv []string) (int, string, string, bool) {
		if slices.Contains(argv, "wait-installation") {
			return 1, "", "timed out", true
		}
		return 0, "", "", false
	}
	if err := ti.Install(ctx, defaultOptions()); err == nil || !strings.Contains(err.Error(), "wasn't installed") {
		t.Fatalf("first Install = %v", err)
	}
	s, err := settings.Load(ti.SettingsPath)
	if err != nil || len(s.GitHub.Apps) != 1 || s.GitHub.Apps[0].ClientID != "Iv23liEXAMPLE" {
		t.Fatalf("settings after the first run: %+v, %v", s, err)
	}

	// The second run continues: no second App, the same VMs, and it refuses new settings.
	ti.node.controller = nil
	opts := defaultOptions()
	opts.FlagsGiven = true
	if err := ti.Install(ctx, opts); err == nil || !strings.Contains(err.Error(), "earlier install's settings") {
		t.Errorf("Install with new settings = %v", err)
	}
	vmsBefore := len(ti.node.vms)
	if err := ti.Install(ctx, defaultOptions()); err != nil {
		t.Fatalf("second Install: %v\n%s", err, ti.stderr)
	}
	if s, _ := settings.Load(ti.SettingsPath); len(s.GitHub.Apps) != 1 {
		t.Errorf("the second run created another App: %+v", s.GitHub.Apps)
	}
	if len(ti.node.vms) != vmsBefore {
		t.Errorf("VMs: %d before, %d after", vmsBefore, len(ti.node.vms))
	}
	if !strings.Contains(ti.stdout.String(), "found an earlier install; continuing it") {
		t.Errorf("output:\n%s", ti.stdout)
	}
}

func TestInstallWithAnExistingApp(t *testing.T) {
	ti := newTestInstaller(t)
	ti.Term = &linkAnswerer{ti: ti}
	opts := defaultOptions()
	opts.App, opts.ClientID = AppManual, "Iv23liEXISTING"
	err := ti.Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	ti.assertNoSecrets(t, err)
	if ti.stdout.Len() > 0 && strings.Contains(ti.stdout.String(), "Created GitHub App") {
		t.Error("created an App")
	}
	if ti.node.credentials["parcon.github-app-Iv23liEXISTING"] != appKeyPEM {
		t.Error("the key wasn't encrypted from stdin")
	}
	if s, _ := settings.Load(ti.SettingsPath); len(s.GitHub.Apps) != 1 || s.GitHub.Apps[0].ClientID != "Iv23liEXISTING" ||
		s.GitHub.Apps[0].Owner != "my-org" {
		t.Errorf("apps = %+v, want the existing App, owned by where it is installed", s.GitHub.Apps)
	}
	// Without --github-url, the owner of an existing App is unknown, so wait-installation is left to refuse an App
	// installed on several accounts.
	if !slices.ContainsFunc(ti.node.lines(), func(l string) bool {
		return strings.HasSuffix(l, "wait-installation -client-id Iv23liEXISTING")
	}) {
		t.Errorf("wait-installation was bound to an account:\n%s", strings.Join(ti.node.lines(), "\n"))
	}
}

func TestInstallStopsOnFailedChecks(t *testing.T) {
	ti := newTestInstaller(t)
	ti.node.storage.Avail = 49 << 30
	ti.node.pending = []map[string]string{{"zone": "other", "state": "new"}}
	err := ti.Install(context.Background(), defaultOptions())
	if !errors.Is(err, errPreflight) {
		t.Fatalf("Install = %v", err)
	}
	out := ti.stdout.String()
	for _, want := range []string{"FAIL  free space on local-lvm: 49 GiB free, 50 GiB needed",
		"FAIL  no pending SDN changes: pending SDN changes to other"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if ti.node.ran("pveum pool add") {
		t.Error("a failed check still changed the node")
	}
}

func TestInstallNeedsATerminalOrYes(t *testing.T) {
	ti := newTestInstaller(t)
	ti.term.NoTTY = true
	err := ti.Install(context.Background(), defaultOptions())
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("Install = %v", err)
	}
	ti.term.Answers = []string{"n"}
	ti.term.NoTTY = false
	if err := ti.Install(context.Background(), defaultOptions()); !errors.Is(err, ErrCanceled) {
		t.Errorf("declined Install = %v", err)
	}
}

func TestUninstall(t *testing.T) {
	ti := newTestInstaller(t)
	ti.answerManifest()
	ctx := context.Background()
	must(t, ti.Install(ctx, defaultOptions()))
	// A worker the controller made, and a VM that isn't ours.
	ti.node.vms[10000] = &fakeVM{id: 10000, pool: RunnerPool, tags: []string{vmtags.Managed, vmtags.Worker},
		running: true, config: map[string]string{}}
	ti.node.vms[500] = &fakeVM{id: 500, pool: "", tags: []string{"mine"}, config: map[string]string{}}
	ti.node.zones = append(ti.node.zones, "other")

	ti.Change.DryRun = true
	must(t, ti.Uninstall(ctx))
	if !strings.Contains(ti.stdout.String(), "now: 10000 10096") {
		t.Errorf("dry-run plan:\n%s", ti.stdout)
	}
	if len(ti.node.vms) != 3 {
		t.Error("the dry run destroyed VMs")
	}

	ti.Change.DryRun = false
	ti.stdout.Reset()
	ti.node.calls = nil
	must(t, ti.Uninstall(ctx))
	if ids := slices.Collect(func(yield func(int) bool) {
		for id := range ti.node.vms {
			if !yield(id) {
				return
			}
		}
	}); len(ids) != 1 || ids[0] != 500 {
		t.Errorf("VMs left: %v", ids)
	}
	n := ti.node
	if len(n.pools) != 0 || len(n.roles) != 0 || len(n.users) != 0 || len(n.acls) != 0 ||
		!slices.Equal(n.zones, []string{"other"}) || len(n.vnets) != 0 || len(n.subnets) != 0 {
		t.Errorf("left: pools %v roles %v users %v acls %v zones %v vnets %v subnets %v", n.pools, n.roles,
			n.users, n.acls, n.zones, n.vnets, n.subnets)
	}
	// The worker firewall goes; the datacenter firewall stays on.
	if len(n.ipsets) != 0 || len(n.groups) != 0 || len(n.hostRules) != 0 || !n.clusterFW {
		t.Errorf("left: IP sets %v, groups %v, host rules %v, datacenter firewall %v", n.ipsets, n.groups,
			n.hostRules, n.clusterFW)
	}
	// Clones before templates, and the scale set is deleted before the VMs go.
	lines := strings.Join(n.lines(), "\n")
	if i, j := strings.Index(lines, "qm destroy 10000"), strings.Index(lines, "qm destroy 10096"); i < 0 || j < i {
		t.Errorf("destroy order:\n%s", lines)
	}
	if i, j := strings.Index(lines, "parcon github scaleset delete"), strings.Index(lines, "qm destroy"); i < 0 || j < i {
		t.Error("the scale set wasn't deleted first")
	}
	for _, path := range []string{ti.SettingsPath, ti.BinaryPath, ti.UnitPath, ti.ConfigPath, ti.CACertPath,
		filepath.Join(ti.CredStoreDir, "parcon.pve-token"),
		filepath.Join(ti.CredStoreDir, "parcon.github-app-Iv23liEXAMPLE")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is left", path)
		}
	}
	if n.services[ServiceName] {
		t.Error("parcon.service is still enabled")
	}
	// The scale sets are deleted before the controller stops.
	if i, j := strings.Index(lines, "github scaleset delete"), strings.Index(lines, "systemctl disable --now parcon.service"); i < 0 || j < i {
		t.Errorf("stop order:\n%s", lines)
	}
}

func TestControllerStopTimeoutOutlastsTheService(t *testing.T) {
	unit := newTestInstaller(t).renderUnit()
	m := regexp.MustCompile(`(?m)^TimeoutStopSec=(\d+)(min|s)$`).FindSubmatch(unit)
	if m == nil {
		t.Fatal("parcon.service has no TimeoutStopSec")
	}
	d, _ := time.ParseDuration(string(m[1]) + map[string]string{"min": "m", "s": "s"}[string(m[2])])
	if ControllerStopTimeout <= d {
		t.Errorf("ControllerStopTimeout %s isn't longer than TimeoutStopSec %s", ControllerStopTimeout, d)
	}
}

// lastLine is the last line of output that isn't blank.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, " \n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// While install waits for the App to be installed, the last thing on the screen is the link to install it.
func TestInstallEndsWithTheAppsInstallLink(t *testing.T) {
	ti := newTestInstaller(t)
	ti.answerManifest()
	var shown []string
	waits := 0
	ti.node.controller = func(argv []string) (int, string, string, bool) {
		if slices.Contains(argv, "wait-installation") {
			shown = append(shown, lastLine(ti.stdout.String()))
			if waits++; waits == 1 {
				return 1, "", "timed out", true // the first run stops here, as if the user didn't install it
			}
		}
		return 0, "", "", false
	}
	if err := ti.Install(context.Background(), defaultOptions()); err == nil {
		t.Fatal("the first run didn't stop")
	}
	// The re-run knows the App's slug from the settings, so it shows the link again.
	if err := ti.Install(context.Background(), defaultOptions()); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	link := "https://github.com/apps/my-runners/installations/new"
	if !slices.Equal(shown, []string{link, link}) {
		t.Errorf("last line while waiting = %q, want the link", shown)
	}
	if s, _ := settings.Load(ti.SettingsPath); s.GitHub.Apps[0].Slug != "my-runners" {
		t.Errorf("slug = %q", s.GitHub.Apps[0].Slug)
	}
}

// An existing App's link isn't known, so install says where to install it instead, last.
func TestInstallWithAnExistingAppSaysWhereToInstallIt(t *testing.T) {
	ti := newTestInstaller(t)
	ti.Term = &linkAnswerer{ti: ti}
	var shown string
	ti.node.controller = func(argv []string) (int, string, string, bool) {
		if slices.Contains(argv, "wait-installation") && shown == "" {
			shown = lastLine(ti.stdout.String())
		}
		return 0, "", "", false
	}
	opts := defaultOptions()
	opts.App, opts.ClientID = AppManual, "Iv23liEXISTING"
	must(t, ti.Install(context.Background(), opts))
	if !strings.HasPrefix(shown, "App), unless it is installed already.") {
		t.Errorf("last line while waiting = %q", shown)
	}
}
