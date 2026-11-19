package settings

import "github.com/klponce/proxmox-actions-runners/internal/config"

// settingsV2 is the settings format before several GitHub targets: one organization or repository, and its App.
type settingsV2 struct {
	Version  int           `yaml:"version"`
	Network  Network       `yaml:"network"`
	Proxmox  Proxmox       `yaml:"proxmox"`
	GitHub   githubV2      `yaml:"github,omitempty"`
	ScaleSet ScaleSet      `yaml:"scaleSet"`
	Worker   config.Worker `yaml:"worker,omitempty"`
}

type githubV2 struct {
	ConfigURL string `yaml:"configUrl,omitempty"`
	App       struct {
		ClientID       string `yaml:"clientId,omitempty"`
		InstallationID int64  `yaml:"installationId,omitempty"`
		Slug           string `yaml:"slug,omitempty"`
	} `yaml:"app,omitempty"`
}

// migrate turns version 2 settings into the current format: the App, if the install got that far, becomes the one App,
// owned by the target's account, and the organization or repository the one target.
func (old settingsV2) migrate() Settings {
	s := Settings{Version: Version, Network: old.Network, Proxmox: old.Proxmox, ScaleSet: old.ScaleSet,
		Worker: old.Worker, migratedFrom: 2}
	g := old.GitHub
	if g.App.ClientID != "" {
		owner, _, _ := config.ParseGitHubURL(g.ConfigURL)
		s.GitHub.Apps = []App{{ClientID: g.App.ClientID, Slug: g.App.Slug, Owner: owner}}
	}
	if g.ConfigURL != "" {
		s.GitHub.Targets = []Target{{URL: g.ConfigURL, ClientID: g.App.ClientID, InstallationID: g.App.InstallationID}}
	}
	return s
}
