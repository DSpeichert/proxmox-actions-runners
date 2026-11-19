package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// fakeScaleSets knows some scale sets by name and records what is deleted.
type fakeScaleSets struct {
	ids       map[string]int
	deleted   []int
	deleteErr error
	// groups records the runner group each lookup asked for.
	groups []string
}

func (f *fakeScaleSets) FindScaleSet(_ context.Context, spec github.ScaleSetSpec) (*github.ScaleSet, error) {
	f.groups = append(f.groups, spec.RunnerGroup)
	id, ok := f.ids[spec.Name]
	if !ok {
		return nil, nil
	}
	return &github.ScaleSet{ID: id, Name: spec.Name}, nil
}

func (f *fakeScaleSets) DeleteScaleSet(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

const (
	orgTarget  = "https://github.com/my-org"
	repoTarget = "https://github.com/octocat/hello"
)

// twoTargets is a github section with an organization and a personal repository, each with its own App.
const twoTargets = `github:
  apps:
    - {clientId: Iv23liORG, privateKeyFile: /etc/org.pem}
    - {clientId: Iv23liUSER, privateKeyFile: /etc/user.pem}
  targets:
    - {configUrl: ` + orgTarget + `, clientId: Iv23liORG, installationId: 1}
    - {configUrl: ` + repoTarget + `, clientId: Iv23liUSER, installationId: 2}
`

func writeScaleSetConfig(t *testing.T, github string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := `proxmox:
  url: https://127.0.0.1:8006/api2/json
  tokenId: par@pve!controller
  tokenSecretFile: /etc/pve-token
  tlsFingerprint: "` + strings.Repeat("AA:", 31) + `AA"
  node: pve1
  pool: par-runners
  storage: local-lvm
  vnet: parnet
  vmidRange: {start: 10000, end: 10999}
` + github + `
scaleSets:
  - {name: small, maxRunners: 2, runnerGroup: builds}
  - {name: large, maxRunners: 1, runnerGroup: builds}
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// useFakeScaleSets serves each target, by key, with its fake.
func useFakeScaleSets(t *testing.T, fakes map[string]*fakeScaleSets) {
	t.Helper()
	old := newScaleSetClients
	t.Cleanup(func() { newScaleSetClients = old })
	newScaleSetClients = func(*config.Config) (map[string]scaleSetClient, error) {
		clients := map[string]scaleSetClient{}
		for k, f := range fakes {
			clients[k] = f
		}
		return clients, nil
	}
}

func TestScaleSetDelete(t *testing.T) {
	org := &fakeScaleSets{ids: map[string]int{"small": 42}}
	repo := &fakeScaleSets{ids: map[string]int{"small": 7, "large": 8}}
	useFakeScaleSets(t, map[string]*fakeScaleSets{"my-org": org, "octocat.hello": repo})
	path := writeScaleSetConfig(t, twoTargets)

	var stdout, stderr bytes.Buffer
	if err := run([]string{"github", "scaleset", "delete", "-config", path}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	for _, want := range []string{
		orgTarget + ": scale set small: deleted (ID 42)", orgTarget + ": scale set large: not registered",
		repoTarget + ": scale set small: deleted (ID 7)", repoTarget + ": scale set large: deleted (ID 8)",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output doesn't contain %q:\n%s", want, stdout.String())
		}
	}
	if !slices.Equal(org.deleted, []int{42}) || !slices.Equal(repo.deleted, []int{7, 8}) {
		t.Errorf("deleted = %v and %v, want [42] and [7 8]", org.deleted, repo.deleted)
	}
	// A repository has no runner groups: its scale sets are in the default one whatever the config says.
	if !slices.Equal(org.groups, []string{"builds", "builds"}) ||
		!slices.Equal(repo.groups, []string{"default", "default"}) {
		t.Errorf("runner groups = %v and %v", org.groups, repo.groups)
	}
}

func TestScaleSetDeleteOneTarget(t *testing.T) {
	org := &fakeScaleSets{ids: map[string]int{"small": 42}}
	repo := &fakeScaleSets{ids: map[string]int{"small": 7}}
	useFakeScaleSets(t, map[string]*fakeScaleSets{"my-org": org, "octocat.hello": repo})
	path := writeScaleSetConfig(t, twoTargets)

	var stdout, stderr bytes.Buffer
	err := run([]string{"github", "scaleset", "delete", "-config", path, "-target", "https://github.com/Octocat/hello/"},
		&stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	if len(org.deleted) != 0 || !slices.Equal(repo.deleted, []int{7}) {
		t.Errorf("deleted = %v and %v, want only the repository's", org.deleted, repo.deleted)
	}

	err = run([]string{"github", "scaleset", "delete", "-config", path, "-target", "https://github.com/elsewhere"},
		&stdout, &stderr)
	if outcomeOf(err) != failed || !strings.Contains(err.Error(), "isn't a target") {
		t.Errorf("an unknown target: %v", err)
	}
}

func TestScaleSetDeleteFailures(t *testing.T) {
	tests := []struct {
		name    string
		github  string
		fakes   map[string]*fakeScaleSets
		wantErr string
	}{
		// The real client builder, which requires the App.
		{"no target yet", "", nil, "no GitHub target is set up yet"},
		{"GitHub refuses one target", twoTargets, map[string]*fakeScaleSets{
			"my-org":        {ids: map[string]int{"small": 42}, deleteErr: errors.New("403 Forbidden")},
			"octocat.hello": {ids: map[string]int{"small": 7}},
		}, orgTarget + ": scale set small: 403 Forbidden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fakes != nil {
				useFakeScaleSets(t, tt.fakes)
			}
			var stdout, stderr bytes.Buffer
			err := run([]string{"github", "scaleset", "delete", "-config", writeScaleSetConfig(t, tt.github)},
				&stdout, &stderr)
			if outcomeOf(err) != failed || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want one containing %q", err, tt.wantErr)
			}
			// The other target's scale sets are deleted all the same.
			if f := tt.fakes["octocat.hello"]; f != nil && !slices.Equal(f.deleted, []int{7}) {
				t.Errorf("the other target's deletions = %v, want [7]", f.deleted)
			}
		})
	}
}
