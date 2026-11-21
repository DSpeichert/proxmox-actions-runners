package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// signedSource serves releases whose SHA256SUMS a test key signs.
type signedSource struct {
	releases []release.Info
	files    map[string][]byte // "version/asset"
}

func (s *signedSource) Releases(context.Context) ([]release.Info, error) { return s.releases, nil }

func (s *signedSource) Open(_ context.Context, v release.Version, asset string) (io.ReadCloser, error) {
	data, ok := s.files[v.String()+"/"+asset]
	if !ok {
		return nil, fmt.Errorf("download %s: 404 Not Found", asset)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// publish adds a signed release with the usual assets.
func (s *signedSource) publish(t *testing.T, key ed25519.PrivateKey, version string) release.Version {
	t.Helper()
	v, err := release.ParseVersion(version)
	must(t, err)
	assets := map[string]string{
		runnerImage(v):    "runner image " + version,
		runnerManifest(v): `{"builds":[{"custom_data":{"runner_version":"2.339.0"}}]}`,
		BinaryAsset(v):    "parcon " + version,
	}
	var sums strings.Builder
	for name, content := range assets {
		sum := sha256.Sum256([]byte(content))
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		s.files[version+"/"+name] = []byte(content)
	}
	s.files[version+"/SHA256SUMS"] = []byte(sums.String())
	s.files[version+"/SHA256SUMS.sig"] = ed25519.Sign(key, []byte(sums.String()))
	s.releases = append(s.releases, release.Info{Version: v})
	return v
}

func TestUpdateInstallsTheNewParconAndRunsIt(t *testing.T) {
	ti := installed(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	src := &signedSource{files: map[string][]byte{}}
	src.publish(t, priv, "0.2.0")
	src.publish(t, priv, "0.3.0-rc.1")
	ti.Source, ti.Keys, ti.Assets = src, []ed25519.PublicKey{pub}, ""
	ti.Yes = true
	var execed []string
	execBinary = func(path string, args []string) error { execed = append([]string{path}, args...); return nil }
	t.Cleanup(func() { execBinary = defaultExecBinary })

	// Without --pre, 0.2.0 is the newest, and the node is at it.
	must(t, ti.Update(context.Background(), UpdateOptions{}))
	if execed != nil || !strings.Contains(ti.stdout.String(), "nothing needs updating") {
		t.Fatalf("update without --pre: exec %v\n%s", execed, ti.stdout)
	}

	// With --pre, parcon 0.3.0-rc.1 replaces this one and finishes the update.
	must(t, ti.Update(context.Background(), UpdateOptions{Pre: true}))
	if data, _ := os.ReadFile(ti.BinaryPath); string(data) != "parcon 0.3.0-rc.1" {
		t.Errorf("host binary = %q", data)
	}
	want := []string{ti.BinaryPath, ti.BinaryPath, "update", "--continue", "--yes", "--pre"}
	if !slices.Equal(execed, want) {
		t.Errorf("exec %v, want %v", execed, want)
	}
}

func TestUpdateRefusesAnUnsignedRelease(t *testing.T) {
	ti := installed(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	src := &signedSource{files: map[string][]byte{}}
	src.publish(t, other, "0.3.0")
	ti.Source, ti.Keys, ti.Assets, ti.Yes = src, []ed25519.PublicKey{pub}, "", true
	_ = priv
	before, _ := os.ReadFile(ti.BinaryPath)
	err := ti.Update(context.Background(), UpdateOptions{})
	if err == nil || !strings.Contains(err.Error(), "doesn't verify") {
		t.Errorf("Update = %v", err)
	}
	if after, _ := os.ReadFile(ti.BinaryPath); !bytes.Equal(before, after) {
		t.Error("the host binary changed")
	}
}

func TestUpgradeBringsTheNodeToTheRelease(t *testing.T) {
	ti := installed(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	src := &signedSource{files: map[string][]byte{}}
	v := src.publish(t, priv, "0.3.0")
	ti.Source, ti.Keys, ti.Assets, ti.Version, ti.Yes = src, []ed25519.PublicKey{pub}, "", v, true
	// The new parcon, as an update runs it: the old one installed it in place first.
	must(t, os.WriteFile(ti.BinaryPath, []byte("parcon 0.3.0"), 0o755))
	ti.Self = ti.BinaryPath

	installedV, _ := release.ParseVersion("0.2.0")
	must(t, ti.Upgrade(context.Background(), installedV, true, false))
	out := ti.stdout.String()
	for _, want := range []string{"import the 0.3.0 runner template", "render the controller's config",
		"record release 0.3.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	n := ti.node
	if tmpl := n.vmsTagged("par-release-0.3.0"); len(tmpl) != 1 {
		t.Errorf("templates of 0.3.0: %v", tmpl)
	}
	if s, _ := settings.Load(ti.SettingsPath); s.Installed != "0.3.0" {
		t.Errorf("installed = %q", s.Installed)
	}
	if !n.ran("systemctl try-restart parcon.service") {
		t.Errorf("the controller wasn't restarted: %v", n.lines())
	}

	// Once there, another upgrade has nothing to do.
	n.calls = nil
	ti.stdout.Reset()
	must(t, ti.Upgrade(context.Background(), v, true, false))
	if !strings.Contains(ti.stdout.String(), "nothing needs updating") || n.ran("pvesh create") {
		t.Errorf("second upgrade:\n%s", ti.stdout)
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
