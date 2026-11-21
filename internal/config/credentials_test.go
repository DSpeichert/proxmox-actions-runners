package config

import (
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

func credentialDoc(t *testing.T) map[string]any {
	doc := validDoc()
	set(t, doc, "proxmox.tokenSecretFile", CredentialPath(CredToken))
	set(t, doc, "proxmox.caCertFile", CredentialPath(CredCACert))
	set(t, doc, "github.apps.0.privateKeyFile", CredentialPath(CredAppKey("Iv23liEXAMPLE0000000")))
	return doc
}

func TestCredentialPaths(t *testing.T) {
	data, err := yaml.Marshal(credentialDoc(t))
	if err != nil {
		t.Fatal(err)
	}
	// Outside the service the paths stay as they are, and still validate.
	t.Setenv(credentialsEnv, "")
	c, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Proxmox.TokenSecretFile != "${CREDENTIALS_DIRECTORY}/parcon.pve-token" {
		t.Errorf("tokenSecretFile = %q", c.Proxmox.TokenSecretFile)
	}

	// In the service, Load puts them in the service's credentials directory.
	dir := t.TempDir()
	path := filepath.Join(dir, CredConfig)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentialsEnv, dir)
	if !InService() || Path() != path {
		t.Fatalf("InService %v, Path %q", InService(), Path())
	}
	c, err = Load(Path())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for got, want := range map[string]string{
		c.Proxmox.TokenSecretFile:       filepath.Join(dir, "parcon.pve-token"),
		c.Proxmox.CACertFile:            filepath.Join(dir, "parcon.pve-ca.pem"),
		c.GitHub.Apps[0].PrivateKeyFile: filepath.Join(dir, "parcon.github-app-Iv23liEXAMPLE0000000"),
	} {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

func TestNotInService(t *testing.T) {
	t.Setenv(credentialsEnv, "")
	if InService() || Path() != DefaultPath {
		t.Errorf("InService %v, Path %q", InService(), Path())
	}
	// A credentials directory without the config is another service's.
	t.Setenv(credentialsEnv, t.TempDir())
	if InService() {
		t.Error("InService without parcon.config")
	}
}

func TestRelativePathStillRefused(t *testing.T) {
	doc := validDoc()
	set(t, doc, "proxmox.tokenSecretFile", "pve-token")
	if _, err := parseDoc(t, doc); err == nil {
		t.Error("a relative tokenSecretFile parsed")
	}
}

// systemd gives credentials mode 0440 in the service's private directory; anywhere else a secret file must be 0600.
func TestReadSecretFileAcceptsCredentials(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, CredToken)
	if err := os.WriteFile(cred, []byte("secret\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "pve-token")
	if err := os.WriteFile(other, []byte("secret\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentialsEnv, "")
	if _, err := ReadSecretFile(cred); err == nil {
		t.Error("a 0440 file outside the service was accepted")
	}
	t.Setenv(credentialsEnv, dir)
	if got, err := ReadSecretFile(cred); err != nil || got != "secret" {
		t.Errorf("credential: %q, %v", got, err)
	}
	if _, err := ReadSecretFile(other); err == nil {
		t.Error("a 0440 file outside the credentials directory was accepted")
	}
}
