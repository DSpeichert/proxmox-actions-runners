// Package settings is the Proxmox host's record of how this node's runners are set up: the network, storage, and
// VMIDs the installer used, the GitHub targets and Apps it learned, and the scale set and worker hardware the user
// chose. parcon keeps it in /etc/proxmox-actions-runners/settings.yaml on the host and renders the controller's
// config.yaml from it, so the settings are the source of truth and the controller's config follows them.
//
// Nothing secret is in them: the Apps' private keys and the Proxmox token are systemd credentials, encrypted, which
// only the controller's service gets.
package settings

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
)

// DefaultPath is where the settings live on the host, next to the controller's config.
const DefaultPath = "/etc/proxmox-actions-runners/settings.yaml"

// Version is the settings file's format version. Version 4 is the first with the controller as a service on the
// host; earlier versions had a controller VM, and parcon update can't move one.
const Version = 4

// Settings is the whole settings file.
type Settings struct {
	Version  int      `yaml:"version"`
	Network  Network  `yaml:"network"`
	Proxmox  Proxmox  `yaml:"proxmox"`
	GitHub   GitHub   `yaml:"github,omitempty"`
	ScaleSet ScaleSet `yaml:"scaleSet"`
	// Worker is the worker hardware the user set. A zero field means the GitHub-matching default in internal/config.
	Worker config.Worker `yaml:"worker,omitempty"`
	// Installed is the release of the finished install. parcon install sets it last, so an install that stopped
	// partway has none, and parcon update sets it to the release it updated to.
	Installed string `yaml:"installed,omitempty"`
}

// Network is the host's LAN side, and the worker network.
type Network struct {
	// Bridge is the LAN bridge: the API's address is on it, workers are kept off its LAN, and its NIC's offloads
	// are turned off.
	Bridge string `yaml:"bridge"`
	// PVEAddress is the address the controller reaches the API on. Empty means the host's first address on Bridge.
	PVEAddress string `yaml:"pveAddress,omitempty"`
	// WorkerSubnet is the worker network's IPv4 subnet. The host takes its first address, and SDN DHCP hands out
	// the rest.
	WorkerSubnet string `yaml:"workerSubnet"`
}

// Proxmox is where VMs go.
type Proxmox struct {
	Storage   string           `yaml:"storage"`
	VMIDRange config.VMIDRange `yaml:"vmidRange"`
}

// GitHub is the organizations and repositories the runners serve and the Apps they use, as learned during the install
// and parcon target add.
type GitHub struct {
	// Apps are the GitHub Apps whose keys the controller holds. A private App can only be installed on the
	// account that owns it, so there is one per organization or personal account.
	Apps []App `yaml:"apps,omitempty"`
	// Targets are the organizations and repositories the runners serve. The first is the one parcon install set up.
	Targets []Target `yaml:"targets,omitempty"`
}

// App identifies a GitHub App. Its private key is a systemd credential only the controller gets.
type App struct {
	ClientID string `yaml:"clientId"`
	// Slug is the App's name in its URLs, when parcon created it: it makes the App's install link.
	Slug string `yaml:"slug,omitempty"`
	// Owner is the organization or personal account the App belongs to and is installed on, once known.
	Owner string `yaml:"owner,omitempty"`
}

// Target is an organization or repository the runners serve. ClientID and InstallationID are empty until the App
// is set up and installed.
type Target struct {
	URL            string `yaml:"url"`
	ClientID       string `yaml:"clientId,omitempty"`
	InstallationID int64  `yaml:"installationId,omitempty"`
}

// Owner returns the organization or personal account the target belongs to.
func (t Target) Owner() string {
	owner, _, _ := config.ParseGitHubURL(t.URL)
	return owner
}

// Key returns the target's key (config.TargetKey).
func (t Target) Key() string { return config.TargetKey(t.URL) }

// Complete reports whether the target has its App and installation.
func (t Target) Complete() bool { return t.ClientID != "" && t.InstallationID != 0 }

// Target returns the target for a URL, compared by key, or nil.
func (g *GitHub) Target(url string) *Target {
	key := config.TargetKey(url)
	for i := range g.Targets {
		if key != "" && g.Targets[i].Key() == key {
			return &g.Targets[i]
		}
	}
	return nil
}

// AppByID returns the App with a Client ID, or nil.
func (g *GitHub) AppByID(clientID string) *App {
	for i := range g.Apps {
		if g.Apps[i].ClientID == clientID {
			return &g.Apps[i]
		}
	}
	return nil
}

// AppFor returns the App for an organization or personal account, compared as GitHub does, or nil: the App whose
// owner it is, or the App of a target of that account.
func (g *GitHub) AppFor(owner string) *App {
	if owner == "" {
		return nil
	}
	for i := range g.Apps {
		if strings.EqualFold(g.Apps[i].Owner, owner) {
			return &g.Apps[i]
		}
	}
	for _, t := range g.Targets {
		if t.ClientID != "" && strings.EqualFold(t.Owner(), owner) {
			return g.AppByID(t.ClientID)
		}
	}
	return nil
}

// TargetsOf returns the URLs of the targets an App serves.
func (g *GitHub) TargetsOf(clientID string) []string {
	var urls []string
	for _, t := range g.Targets {
		if t.ClientID == clientID {
			urls = append(urls, t.URL)
		}
	}
	return urls
}

// CompleteTargets returns the targets that have their App and installation, which the controller serves.
func (g *GitHub) CompleteTargets() []Target {
	var out []Target
	for _, t := range g.Targets {
		if t.Complete() {
			out = append(out, t)
		}
	}
	return out
}

// ScaleSet is the one scale set the installer creates.
type ScaleSet struct {
	Name string `yaml:"name"`
	// Labels are the runs-on labels. Empty means just the name.
	Labels      []string `yaml:"labels,omitempty"`
	RunnerGroup string   `yaml:"runnerGroup"`
	MinRunners  int      `yaml:"minRunners"`
	MaxRunners  int      `yaml:"maxRunners"`
	// MaxLifetime is how long a worker may live, job included, before it is destroyed. Zero means
	// config.DefaultMaxLifetime.
	MaxLifetime time.Duration `yaml:"maxLifetime,omitempty"`
}

// Default returns the settings of a default install. The values come from internal/config, the one home of every
// default.
func Default() Settings {
	return Settings{
		Version: Version,
		Network: Network{
			Bridge:       config.DefaultBridge,
			WorkerSubnet: config.DefaultWorkerSubnet,
		},
		Proxmox: Proxmox{
			Storage:   config.DefaultStorage,
			VMIDRange: config.VMIDRange{Start: config.DefaultVMIDStart, End: config.DefaultVMIDEnd},
		},
		ScaleSet: ScaleSet{
			Name:        config.DefaultScaleSetName,
			RunnerGroup: config.DefaultRunnerGroup,
			MinRunners:  config.DefaultMinRunners,
			MaxRunners:  config.DefaultMaxRunners,
		},
	}
}

// Load reads and validates the settings file.
func Load(path string) (*Settings, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the settings path.
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("settings %s: %w", path, err)
	}
	return s, nil
}

// Parse decodes and validates settings. Unknown fields are errors.
func Parse(data []byte) (*Settings, error) {
	// The version comes first: an older version's fields would fail the strict decode below.
	var v struct {
		Version int `yaml:"version"`
	}
	if yaml.Unmarshal(data, &v) == nil && v.Version >= 1 && v.Version < Version {
		// Versions 1 to 3 had a controller VM, and version 1 a gateway VM too. Such an install can't be updated in
		// place.
		return nil, errors.New("these settings are from a release with a controller VM, which this release " +
			"replaces with a service on the host; run parcon uninstall with the release that installed it, then " +
			"parcon install")
	}
	s, err := decodeStrict[Settings](data)
	if err != nil {
		return nil, err
	}
	if s.Version != Version {
		return nil, fmt.Errorf("version %d isn't supported; this parcon reads version %d", s.Version, Version)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

const header = "# parcon's settings for this node. Change them with `parcon config set KEY VALUE`, which also updates\n" +
	"# the controller; see `parcon config get --all`.\n"

// decodeStrict decodes one YAML document into a T, failing on unknown fields.
func decodeStrict[T any](data []byte) (T, error) {
	var out T
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&out); err != nil {
		if errors.Is(err, io.EOF) {
			return out, errors.New("settings are empty")
		}
		return out, fmt.Errorf("decode: %w", err)
	}
	return out, nil
}

// Marshal encodes the settings as the file parcon writes.
func (s *Settings) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	return buf.Bytes(), nil
}

// Save writes the settings to path atomically, readable only by root.
func (s *Settings) Save(path string) error {
	data, err := s.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return pvecli.WriteFileAtomic(path, data, 0o600)
}

// ValidationError lists every problem in the settings, each prefixed with its field.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid settings:\n  - " + strings.Join(e.Problems, "\n  - ")
}

var (
	// Names end up in Proxmox commands and in YAML, so each is kept to a strict pattern.
	idPattern          = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)
	labelPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	runnerGroupPattern = regexp.MustCompile(`^[A-Za-z0-9._ -]+$`)
	githubURLPattern   = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9-]+(/[A-Za-z0-9._-]+)?$`)
	slugPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	loginPattern       = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

// DHCPLeases is how many addresses a worker subnet's DHCP range holds: all but the network address, the host's
// gateway address, and the broadcast address.
func DHCPLeases(subnet netip.Prefix) int {
	return 1<<(32-subnet.Bits()) - 3
}

// Validate checks every setting and reports every problem.
func (s *Settings) Validate() error {
	var p []string
	add := func(field, format string, args ...any) { p = append(p, field+": "+fmt.Sprintf(format, args...)) }

	n := s.Network
	if !idPattern.MatchString(n.Bridge) {
		add("network.bridge", "%q is not a bridge name", n.Bridge)
	}
	if a, err := netip.ParseAddr(n.PVEAddress); n.PVEAddress != "" && (err != nil || !a.Is4()) {
		add("network.pveAddress", "%q must be an IPv4 address", n.PVEAddress)
	}
	if sub, err := netip.ParsePrefix(n.WorkerSubnet); err != nil || !sub.Addr().Is4() {
		add("network.workerSubnet", "%q must be an IPv4 subnet, such as %s", n.WorkerSubnet, config.DefaultWorkerSubnet)
	} else if sub.Bits() < 8 || sub.Bits() > 29 || sub.Masked() != sub {
		add("network.workerSubnet", "%q must be a network address with a /8 to /29 prefix, such as %s",
			n.WorkerSubnet, sub.Masked())
	} else if leases := DHCPLeases(sub); leases < s.ScaleSet.MaxRunners+config.ReservedVMIDs {
		add("network.workerSubnet", "%s has %d DHCP addresses but needs %d: maxRunners plus %d reserved",
			n.WorkerSubnet, leases, s.ScaleSet.MaxRunners+config.ReservedVMIDs, config.ReservedVMIDs)
	}

	if !idPattern.MatchString(s.Proxmox.Storage) {
		add("proxmox.storage", "%q is not a storage ID", s.Proxmox.Storage)
	}
	r := s.Proxmox.VMIDRange
	switch {
	case r.Start < 100 || r.End > 999999999:
		add("proxmox.vmidRange", "VMIDs must be 100 to 999999999")
	case r.Start > r.End:
		add("proxmox.vmidRange", "start %d is after end %d", r.Start, r.End)
	case r.Size() < s.ScaleSet.MaxRunners+config.ReservedVMIDs:
		add("proxmox.vmidRange", "has %d IDs but needs %d: maxRunners plus %d reserved", r.Size(),
			s.ScaleSet.MaxRunners+config.ReservedVMIDs, config.ReservedVMIDs)
	}

	g := s.GitHub
	apps := map[string]bool{}
	for i, a := range g.Apps {
		field := fmt.Sprintf("github.apps[%d]", i)
		if !labelPattern.MatchString(a.ClientID) {
			add(field+".clientId", "%q is not a Client ID", a.ClientID)
		} else if apps[a.ClientID] {
			add(field+".clientId", "%s is listed twice", a.ClientID)
		}
		apps[a.ClientID] = true
		if a.Slug != "" && !slugPattern.MatchString(a.Slug) {
			add(field+".slug", "%q is not an App's slug", a.Slug)
		}
		if a.Owner != "" && !loginPattern.MatchString(a.Owner) {
			add(field+".owner", "%q is not a GitHub account", a.Owner)
		}
	}
	targets := map[string]bool{}
	installations := map[string]int64{} // by App and owner
	for i, t := range g.Targets {
		field := fmt.Sprintf("github.targets[%d]", i)
		if !githubURLPattern.MatchString(t.URL) {
			add(field+".url", "%q must be https://github.com/<org> or https://github.com/<owner>/<repo>", t.URL)
		} else if targets[t.Key()] {
			add(field+".url", "%s is listed twice", t.URL)
		}
		targets[t.Key()] = true
		if t.ClientID != "" && !apps[t.ClientID] {
			add(field+".clientId", "%q isn't one of github.apps", t.ClientID)
		}
		if t.InstallationID < 0 {
			add(field+".installationId", "must not be negative")
		}
		// An App has one installation per account, which all of the account's targets share.
		if t.InstallationID > 0 {
			k := t.ClientID + "/" + strings.ToLower(t.Owner())
			if id, ok := installations[k]; ok && id != t.InstallationID {
				add(field+".installationId", "%d differs from %d, the App's installation on %s", t.InstallationID, id,
					t.Owner())
			}
			installations[k] = t.InstallationID
		}
	}

	ss := s.ScaleSet
	if !config.ScaleSetNamePattern.MatchString(ss.Name) {
		add("scaleSet.name", "%q must be 1 to 63 lowercase letters, digits, '.', '_', or '-', starting with a letter "+
			"or digit", ss.Name)
	}
	for _, l := range ss.Labels {
		if !labelPattern.MatchString(l) {
			add("scaleSet.labels", "%q must be letters, digits, '.', '_', or '-'", l)
		}
	}
	// A repository's scale sets use the default runner group whatever this says (config.ScaleSet.RunnerGroupFor), so
	// organizations and repositories can be served together.
	if !runnerGroupPattern.MatchString(ss.RunnerGroup) {
		add("scaleSet.runnerGroup", "%q has characters a runner group can't have", ss.RunnerGroup)
	}
	if l := ss.MaxLifetime; l != 0 && (l < config.MinMaxLifetime || l > config.MaxMaxLifetime) {
		add("scaleSet.maxLifetime", "%s must be %s to %s", l, config.FormatLifetime(config.MinMaxLifetime),
			config.FormatLifetime(config.MaxMaxLifetime))
	}
	if ss.MinRunners < 0 {
		add("scaleSet.minRunners", "must not be negative")
	}
	if ss.MaxRunners < 1 {
		add("scaleSet.maxRunners", "must be at least 1")
	} else if ss.MinRunners > ss.MaxRunners {
		add("scaleSet.minRunners", "%d is more than maxRunners %d", ss.MinRunners, ss.MaxRunners)
	}

	if w := s.Worker; w.Cores != 0 && w.Cores < config.MinCores {
		add("worker.cores", "must be at least %d", config.MinCores)
	}
	if w := s.Worker; w.MemoryMiB != 0 && w.MemoryMiB < config.MinMemoryMiB {
		add("worker.memoryMiB", "must be at least %d", config.MinMemoryMiB)
	}
	if s.Worker.FreeDiskGiB < 0 {
		add("worker.freeDiskGiB", "must not be negative")
	}

	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// EffectiveMaxLifetime returns the worker lifetime in effect: the settings' value, or the default.
func (s *Settings) EffectiveMaxLifetime() time.Duration {
	if s.ScaleSet.MaxLifetime == 0 {
		return config.DefaultMaxLifetime
	}
	return s.ScaleSet.MaxLifetime
}

// EffectiveWorker returns the worker hardware in effect: the settings' values, with defaults for those not set.
func (s *Settings) EffectiveWorker() config.Worker {
	w, d := s.Worker, config.DefaultWorker()
	if w.Cores == 0 {
		w.Cores = d.Cores
	}
	if w.MemoryMiB == 0 {
		w.MemoryMiB = d.MemoryMiB
	}
	if w.FreeDiskGiB == 0 {
		w.FreeDiskGiB = d.FreeDiskGiB
	}
	return w
}
