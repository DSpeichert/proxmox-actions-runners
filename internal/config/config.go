// Package config defines the controller's configuration file: its schema, defaults, and validation.
//
// parcon on the Proxmox host renders the file from its settings and writes it to
// /etc/proxmox-actions-runners/config.yaml, which the controller's service gets as a credential. Secrets are never
// stored in it; it only names the credentials that hold them.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// The controller's files on the host (docs/install.md, "Controller service contract"), readable only by root. The
// service gets them as credentials (CredConfig, CredCACert); the secrets are credentials of their own.
const (
	Dir = "/etc/proxmox-actions-runners"
	// DefaultPath is the config file.
	DefaultPath = Dir + "/config.yaml"
	// CACertPath is a copy of the node's CA, when the API serves the node's own certificate.
	CACertPath = Dir + "/pve-ca.pem"
)

// Config is the controller's whole configuration.
type Config struct {
	Proxmox Proxmox `yaml:"proxmox"`
	GitHub  GitHub  `yaml:"github,omitempty"`
	// Worker is the controller-wide worker hardware. Fields left unset use the GitHub-matching defaults.
	Worker Worker `yaml:"worker,omitempty"`
	// MaxWorkers caps the workers of all scale sets together, so scale sets that each may use the whole node can't
	// together overcommit it. Zero means the sum of the scale sets' maxRunners.
	MaxWorkers int        `yaml:"maxWorkers,omitempty"`
	ScaleSets  []ScaleSet `yaml:"scaleSets"`
}

// EffectiveMaxWorkers returns how many workers the controller runs at most: MaxWorkers, or if it is zero, the sum of
// the scale sets' maxRunners.
func (c *Config) EffectiveMaxWorkers() int {
	if c.MaxWorkers > 0 {
		return c.MaxWorkers
	}
	n := 0
	for _, s := range c.ScaleSets {
		n += max(s.MaxRunners, 0)
	}
	return n
}

// Proxmox says how to reach the Proxmox VE API and where workers are created.
type Proxmox struct {
	// URL is the API base URL, for example https://pve.example.com:8006/api2/json. A URL without a path gets
	// /api2/json.
	URL string `yaml:"url"`
	// TokenID is the API token ID, for example par@pve!controller.
	TokenID string `yaml:"tokenId"`
	// TokenSecretFile is the absolute path of the file that holds the token secret.
	TokenSecretFile string `yaml:"tokenSecretFile"`
	// CACertFile is the absolute path of a PEM file with the CA certificates that verify the API's certificate, such
	// as a copy of the node's /etc/pve/pve-root-ca.pem. Empty means the system trust store. Unlike a pinned
	// fingerprint, it keeps working when the certificate is renewed.
	CACertFile string `yaml:"caCertFile,omitempty"`
	// TLSServerName is the name the API's certificate must be issued for, when the URL uses an IP address the
	// certificate doesn't list. Empty means the URL's host.
	TLSServerName string `yaml:"tlsServerName,omitempty"`
	// TLSFingerprint pins the API's certificate by its SHA-256 fingerprint (AA:BB:...) instead of verifying it, for
	// a certificate from a CA the controller can't trust. A renewed certificate breaks the pin.
	TLSFingerprint string `yaml:"tlsFingerprint,omitempty"`
	// Node is the standalone node that runs the workers.
	Node string `yaml:"node"`
	// Pool is the resource pool that holds templates and workers.
	Pool string `yaml:"pool"`
	// Storage is where clones are created.
	Storage string `yaml:"storage"`
	// Zone is the SDN zone that holds VNet. Empty means DefaultZone, the zone the installer creates.
	Zone string `yaml:"zone,omitempty"`
	// VNet is the worker network's SDN VNet.
	VNet string `yaml:"vnet"`
	// FirewallGroup is the datacenter firewall's security group every worker's own firewall must use; the
	// controller starts no worker without it. Empty means WorkerFirewallGroup, the group the installer creates.
	FirewallGroup string `yaml:"firewallGroup,omitempty"`
	// VMIDRange is the range new VMs take their IDs from.
	VMIDRange VMIDRange `yaml:"vmidRange"`
	// LinkedClone chooses linked clones over full clones. Nil means DefaultLinkedClone; Parse always sets it.
	LinkedClone *bool `yaml:"linkedClone,omitempty"`
}

// UseLinkedClone reports whether workers are created as linked clones.
func (p Proxmox) UseLinkedClone() bool {
	if p.LinkedClone == nil {
		return DefaultLinkedClone
	}
	return *p.LinkedClone
}

// VMIDRange is an inclusive range of VM IDs.
type VMIDRange struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

// Size is the number of IDs in the range.
func (r VMIDRange) Size() int {
	if r.End < r.Start {
		return 0
	}
	return r.End - r.Start + 1
}

// Contains reports whether id is in the range.
func (r VMIDRange) Contains(id int) bool {
	return id >= r.Start && id <= r.End
}

// Workers returns the part of the range that worker VMs take their IDs from: everything but the last ReservedVMIDs
// IDs, which hold templates and the installer's smoke-test clones. Keeping those apart from workers lets the
// controller tell a new template, whose template flag Proxmox reports late, from a half-created worker clone, which
// carries the template's tags.
func (r VMIDRange) Workers() VMIDRange {
	return VMIDRange{Start: r.Start, End: r.End - ReservedVMIDs}
}

// GitHub says which organizations and repositories the scale sets serve, and the GitHub Apps that authenticate for
// them. Every scale set is registered with every target.
type GitHub struct {
	// Apps are the GitHub Apps whose private keys the controller holds. A private App can only be installed on the
	// account that owns it, so there is one per organization or personal account.
	Apps []GitHubApp `yaml:"apps,omitempty"`
	// Targets are the organizations and repositories the runners serve.
	Targets []Target `yaml:"targets,omitempty"`

	// ConfigURL and App are the format before several targets: one target and its App. Parse turns them into one
	// App and one target, so a controller reads a config an older release rendered. Marshal never writes them.
	ConfigURL string    `yaml:"configUrl,omitempty"`
	App       LegacyApp `yaml:"app,omitempty"`
}

// GitHubApp is a GitHub App the controller authenticates as.
type GitHubApp struct {
	ClientID string `yaml:"clientId"`
	// PrivateKeyFile is the App's private key (PEM), readable only by the controller.
	PrivateKeyFile string `yaml:"privateKeyFile"`
}

// Target is an organization or repository the runners serve, and the App installation that reaches it.
type Target struct {
	// ConfigURL is https://github.com/<org> or https://github.com/<owner>/<repo>.
	ConfigURL string `yaml:"configUrl"`
	// ClientID names the App in Apps, and InstallationID is where it is installed. Both are empty until the installer
	// learns them.
	ClientID       string `yaml:"clientId,omitempty"`
	InstallationID int64  `yaml:"installationId,omitempty"`
}

// IsRepository reports whether the target is a repository rather than an organization.
func (t Target) IsRepository() bool {
	_, repo, err := ParseGitHubURL(t.ConfigURL)
	return err == nil && repo != ""
}

// Key returns the target's key (TargetKey).
func (t Target) Key() string { return TargetKey(t.ConfigURL) }

// LegacyApp is the App block of the format before several targets.
type LegacyApp struct {
	ClientID       string `yaml:"clientId,omitempty"`
	InstallationID int64  `yaml:"installationId,omitempty"`
	PrivateKeyFile string `yaml:"privateKeyFile,omitempty"`
}

func (g GitHub) legacy() bool { return g.ConfigURL != "" || g.App != LegacyApp{} }

// normalize turns the format before several targets into one App and one target. A config that mixes the two
// formats is left as it is, for validation to report.
func (g *GitHub) normalize() {
	if !g.legacy() || len(g.Apps) > 0 || len(g.Targets) > 0 {
		return
	}
	if g.App.ClientID != "" || g.App.PrivateKeyFile != "" {
		g.Apps = []GitHubApp{{ClientID: g.App.ClientID, PrivateKeyFile: g.App.PrivateKeyFile}}
	}
	if g.ConfigURL != "" {
		g.Targets = []Target{{ConfigURL: g.ConfigURL, ClientID: g.App.ClientID, InstallationID: g.App.InstallationID}}
	}
	g.ConfigURL, g.App = "", LegacyApp{}
}

// AppFor returns the App with the client ID, if the config has it.
func (g GitHub) AppFor(clientID string) (GitHubApp, bool) {
	for _, a := range g.Apps {
		if a.ClientID == clientID {
			return a, true
		}
	}
	return GitHubApp{}, false
}

// RunnerGroupFor returns the runner group the scale set uses for a target. GitHub has runner groups only for
// organizations, so a repository's scale sets use the default group whatever the config says.
func (s ScaleSet) RunnerGroupFor(t Target) string {
	if t.IsRepository() {
		return DefaultRunnerGroup
	}
	return s.RunnerGroup
}

// maxTargetKey is the longest TargetKey, so the worker tag that carries it stays well within what Proxmox accepts.
const maxTargetKey = 48

// TargetKey returns a short, stable name for the organization or repository a GitHub URL names, for VM tags and logs:
// the owner, or owner.repo, in lowercase, as GitHub compares them. GitHub logins and repository names use only
// letters, digits, '.', '_', and '-', all allowed in a Proxmox tag. A key longer than 48 characters is cut short and
// ends with a hash of the whole, so it stays unique. A URL ParseGitHubURL rejects has no key.
func TargetKey(raw string) string {
	owner, repo, err := ParseGitHubURL(raw)
	if err != nil {
		return ""
	}
	key := strings.ToLower(owner)
	if repo != "" {
		key += "." + strings.ToLower(repo)
	}
	if len(key) > maxTargetKey {
		sum := sha256.Sum256([]byte(key))
		key = key[:maxTargetKey-9] + "-" + hex.EncodeToString(sum[:4])
	}
	return key
}

// ParseGitHubURL splits https://github.com/<org> or https://github.com/<owner>/<repo>, with or without a trailing
// slash, into the owner and, for a repository, its name. Anything else is an error: GitHub Enterprise Server and
// enterprise-level scale sets are out of scope (README, "Limitations").
func ParseGitHubURL(raw string) (owner, repo string, err error) {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "https" && u.Host == "github.com" && u.RawQuery == "" && u.Fragment == "" &&
		u.User == nil {
		switch parts := strings.Split(strings.Trim(u.Path, "/"), "/"); {
		case len(parts) == 1 && parts[0] != "":
			return parts[0], "", nil
		case len(parts) == 2 && parts[0] != "" && parts[1] != "":
			return parts[0], parts[1], nil
		}
	}
	return "", "", fmt.Errorf("%q must be https://github.com/<org> or https://github.com/<owner>/<repo>", raw)
}

// Worker is a worker VM's hardware. A zero field means "not set" and is filled from the level above.
type Worker struct {
	Cores     int `yaml:"cores,omitempty"`
	MemoryMiB int `yaml:"memoryMiB,omitempty"`
	// FreeDiskGiB is added on top of the template's disk size.
	FreeDiskGiB int `yaml:"freeDiskGiB,omitempty"`
}

// ScaleSet is one GitHub runner scale set served by the controller.
type ScaleSet struct {
	Name string `yaml:"name"`
	// Labels are what workflows put in runs-on. Empty means just the name.
	Labels []string `yaml:"labels,omitempty"`
	// RunnerGroup is the GitHub runner group the scale set belongs to, which controls which repositories and
	// workflows may use it. Empty means DefaultRunnerGroup. Repository scale sets must use the default group.
	RunnerGroup string        `yaml:"runnerGroup,omitempty"`
	MinRunners  int           `yaml:"minRunners,omitempty"`
	MaxRunners  int           `yaml:"maxRunners"`
	MaxLifetime time.Duration `yaml:"maxLifetime,omitempty"`
	// Worker overrides the controller-wide worker hardware. After Parse, every field is set.
	Worker Worker `yaml:"worker,omitempty"`
}

// Marshal encodes a config as a YAML document that Parse reads back to the same config. Unset optional fields are
// left out, so defaults stay defaults.
func Marshal(c *Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

// Load reads and parses the config file at path. In the service, paths in the credentials directory
// (CredentialsDir) become paths in the service's own.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator chooses the config path.
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.expandCredentials()
	return c, nil
}

// Parse decodes a config document, fills in defaults, and validates the result. Unknown fields are errors, so a
// typo can't silently fall back to a default. A validation failure is a *ValidationError listing every problem.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, fmt.Errorf("decode: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("config must contain exactly one YAML document")
	}

	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
