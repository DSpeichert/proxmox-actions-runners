package installer

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// The controller runs with no privileges, no capabilities, a read-only system, and none of Proxmox's secrets or
// ours in reach: only its own credentials.
func TestUnitSandboxesTheController(t *testing.T) {
	ti := newTestInstaller(t)
	unit := string(ti.renderUnit())
	for _, want := range []string{
		"ExecStart=" + ti.BinaryPath + " run\n", "DynamicUser=yes\n", "PrivateUsers=yes\n",
		"CapabilityBoundingSet=\n", "AmbientCapabilities=\n", "NoNewPrivileges=yes\n", "ProtectSystem=strict\n",
		"ProtectHome=yes\n", "PrivateTmp=yes\n", "PrivateDevices=yes\n", "ProtectKernelTunables=yes\n",
		"ProtectKernelModules=yes\n", "ProtectControlGroups=yes\n", "RestrictNamespaces=yes\n",
		"RestrictSUIDSGID=yes\n", "MemoryDenyWriteExecute=yes\n", "SystemCallFilter=@system-service\n",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX\n", "UMask=0077\n", "MemoryMax=512M\n",
		"LoadCredential=parcon.config:" + ti.ConfigPath + "\n", "ImportCredential=parcon.*\n",
		"RuntimeDirectory=parcon\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	for _, path := range []string{"/etc/pve", "/etc/credstore.encrypted", "/etc/proxmox-actions-runners", "/root",
		"/var/lib/systemd/credential.secret"} {
		if !strings.Contains(unit, "-"+path+" ") && !strings.Contains(unit, "-"+path+"\n") {
			t.Errorf("unit leaves %s in reach:\n%s", path, unit)
		}
	}
	// The node's CA comes along only when there is one.
	if strings.Contains(unit, "parcon.pve-ca.pem") {
		t.Errorf("unit loads a CA that isn't there:\n%s", unit)
	}
	must(t, os.MkdirAll(ti.dir+"/etc", 0o700))
	must(t, os.WriteFile(ti.CACertPath, []byte("CA"), 0o600))
	if !strings.Contains(string(ti.renderUnit()), "LoadCredential=parcon.pve-ca.pem:"+ti.CACertPath+"\n") {
		t.Error("unit doesn't load the CA")
	}
}

// The host runs the controller's commands in the same sandbox as the service, bounded in time.
func TestControllerExecUsesTheServicesSandbox(t *testing.T) {
	ti := newTestInstaller(t)
	_, err := ti.controllerExec(context.Background(), 0, "check", "config")
	must(t, err)
	l := ti.node.lines()[0]
	for _, p := range ti.sandboxProperties() {
		if !strings.Contains(l, "--property="+p) {
			t.Errorf("systemd-run lacks %s: %s", p, l)
		}
	}
	if !strings.Contains(l, "--wait") || !strings.Contains(l, "--pipe") ||
		!strings.Contains(l, "--property=RuntimeMaxSec=") || !strings.HasSuffix(l, "-- "+ti.BinaryPath+" check config") {
		t.Errorf("systemd-run = %s", l)
	}
}

func TestCheckCredentials(t *testing.T) {
	ti := newTestInstaller(t)
	s := settings.Default()
	c := check(t, ti.Preflight(context.Background(), &s, ModeCheck), "systemd credentials")
	if !c.OK || !strings.Contains(c.Reason, "systemd 257; no TPM2") {
		t.Errorf("without a TPM2: %+v", c)
	}
	ti.node.tpm2 = true
	c = check(t, ti.Preflight(context.Background(), &s, ModeCheck), "systemd credentials")
	if !c.OK || !strings.Contains(c.Reason, "with the TPM2 and the host's key") {
		t.Errorf("with a TPM2: %+v", c)
	}
	// Preflight changes nothing: it never encrypts.
	if slices.ContainsFunc(ti.node.lines(), func(l string) bool { return strings.HasPrefix(l, "systemd-creds encrypt") }) {
		t.Error("preflight encrypted a credential")
	}
}
