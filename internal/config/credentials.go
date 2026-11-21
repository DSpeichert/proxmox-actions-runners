package config

import (
	"os"
	"path/filepath"
	"strings"
)

// The controller runs as parcon.service on the Proxmox host. systemd hands it the config and its secrets as
// credentials, in a directory only the service can read, named by $CREDENTIALS_DIRECTORY. The config names its
// secret files relative to that directory with CredentialsDir, which Load expands.
//
//nolint:gosec // G101: these are the credentials' names, not their values.
const (
	// CredentialsDir stands for the service's credentials directory in a path the config names.
	CredentialsDir = "${CREDENTIALS_DIRECTORY}"
	// credentialsEnv is the variable systemd sets to the credentials directory.
	credentialsEnv = "CREDENTIALS_DIRECTORY"

	// CredConfig is the config's credential name.
	CredConfig = "parcon.config"
	// CredCACert is the node's CA, when the API serves the node's own certificate. It isn't secret.
	CredCACert = "parcon.pve-ca.pem"
	// CredToken is the Proxmox API token's secret.
	CredToken = "parcon.pve-token"
	// CredPrefix starts the name of every credential the controller gets.
	CredPrefix = "parcon."
)

// CredAppKey is the credential name of a GitHub App's private key, by its Client ID.
func CredAppKey(clientID string) string { return CredPrefix + "github-app-" + clientID }

// CredentialPath is the path a config gives for credential name: inside the service's credentials directory.
func CredentialPath(name string) string { return CredentialsDir + "/" + name }

// credentialsDir returns the service's credentials directory, or "" outside the service.
func credentialsDir() string { return os.Getenv(credentialsEnv) }

// InService reports whether parcon runs with the controller's credentials: as parcon.service, or as a command the
// host runs in its sandbox.
func InService() bool {
	dir := credentialsDir()
	if dir == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, CredConfig))
	return err == nil && st.Mode().IsRegular()
}

// Path returns the config the controller reads by default: its credential in the service, otherwise the file on the
// host.
func Path() string {
	if dir := credentialsDir(); dir != "" {
		return filepath.Join(dir, CredConfig)
	}
	return DefaultPath
}

// isCredentialPath reports whether path is inside the credentials directory.
func isCredentialPath(path string) bool { return strings.HasPrefix(path, CredentialsDir+"/") }

// expandCredentials replaces CredentialsDir in the config's file paths with the service's credentials directory.
// Outside the service such a path stays as it is, and reading it fails with its name.
func (c *Config) expandCredentials() {
	dir := credentialsDir()
	if dir == "" {
		return
	}
	expand := func(p *string) {
		if isCredentialPath(*p) {
			*p = filepath.Join(dir, strings.TrimPrefix(*p, CredentialsDir+"/"))
		}
	}
	expand(&c.Proxmox.TokenSecretFile)
	expand(&c.Proxmox.CACertFile)
	for i := range c.GitHub.Apps {
		expand(&c.GitHub.Apps[i].PrivateKeyFile)
	}
}
