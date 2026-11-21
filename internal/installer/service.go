package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// The controller runs on the host as parcon.service (docs/install.md, "Controller service contract"): a transient
// user (DynamicUser) in a systemd sandbox, with its config and secrets as systemd credentials. The secrets are
// encrypted with systemd-creds, with the host's key and the TPM2 if the node has one; only the service, and the
// commands the host runs in the same sandbox, get them decrypted, in a directory nothing else can read.
const (
	// ServiceName is the controller's unit.
	ServiceName = "parcon.service"
	// UnitPath is where parcon writes the unit.
	UnitPath = "/etc/systemd/system/" + ServiceName
	// CredStoreDir holds the encrypted credentials, where ImportCredential= finds them.
	CredStoreDir = "/etc/credstore.encrypted" //nolint:gosec // G101: a directory, not a credential.
)

// sandboxProperties are the systemd properties the controller runs with, as parcon.service and in every command the
// host runs as the controller (controllerExec). The unit's own properties come on top. Keep them the strictest that
// lets the controller work: `systemd-analyze security parcon.service` checks them on a node (test/integration).
func (in *Installer) sandboxProperties() []string {
	// Its config and secrets: the config and the node's CA from our directory, the secrets encrypted in the
	// credential store.
	creds := []string{"LoadCredential=" + config.CredConfig + ":" + in.ConfigPath}
	if _, err := os.Stat(in.CACertPath); err == nil {
		creds = append(creds, "LoadCredential="+config.CredCACert+":"+in.CACertPath)
	}
	creds = append(creds, "ImportCredential="+config.CredPrefix+"*")
	return append(creds,
		// A user that exists only while the controller runs, owns no file outside its runtime directory, and has no
		// capabilities.
		"DynamicUser=yes",
		"PrivateUsers=yes",
		"CapabilityBoundingSet=",
		"AmbientCapabilities=",
		"NoNewPrivileges=yes",
		// The file system: read-only, and none of Proxmox's secrets, ours, or root's.
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"PrivateIPC=yes",
		"InaccessiblePaths=-/etc/pve -/var/lib/pve-cluster -/etc/proxmox-actions-runners -/etc/credstore "+
			"-/etc/credstore.encrypted -/var/lib/systemd/credential.secret -/root",
		// The kernel and the rest of the system.
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectKernelLogs=yes",
		"ProtectControlGroups=yes",
		"ProtectClock=yes",
		"ProtectHostname=yes",
		"ProtectProc=invisible",
		"ProcSubset=pid",
		"RestrictNamespaces=yes",
		"RestrictRealtime=yes",
		"RestrictSUIDSGID=yes",
		"LockPersonality=yes",
		"MemoryDenyWriteExecute=yes",
		"RemoveIPC=yes",
		"UMask=0077",
		// It talks HTTPS to GitHub and the Proxmox API, and logs to the journal.
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
		"SystemCallArchitectures=native",
		"SystemCallFilter=@system-service",
		"SystemCallFilter=~@privileged @resources",
		"SystemCallErrorNumber=EPERM",
		// Bounds, so a runaway controller can't starve the node's VMs.
		"MemoryMax=512M",
		"TasksMax=256",
	)
}

// renderUnit returns parcon.service.
func (in *Installer) renderUnit() []byte {
	var b strings.Builder
	b.WriteString(`# Written by parcon install and parcon update; parcon uninstall removes it. Don't edit it: parcon rewrites it.
[Unit]
Description=proxmox-actions-runners controller
Documentation=https://github.com/klponce/proxmox-actions-runners
Wants=network-online.target
# GitHub App tokens need a correct clock, and the controller the Proxmox API.
After=network-online.target time-sync.target pveproxy.service

[Service]
Type=exec
ExecStart=` + in.BinaryPath + ` run
Restart=on-failure
RestartSec=10s
# Room for the work in flight to finish after SIGTERM: up to 30s to close the GitHub message sessions, then up to
# 5min for retirements (retireTimeout in internal/controller/worker.go).
TimeoutStopSec=6min
# /run/parcon, for the status file parcon status reads (internal/controller/status.go).
RuntimeDirectory=parcon
RuntimeDirectoryMode=0700
`)
	for _, p := range in.sandboxProperties() {
		b.WriteString(p + "\n")
	}
	b.WriteString(`
[Install]
WantedBy=multi-user.target
`)
	return []byte(b.String())
}

// writeUnit writes parcon.service if it differs from what parcon renders, and has systemd read it again.
func (in *Installer) writeUnit(ctx context.Context) error {
	want := in.renderUnit()
	if have, err := os.ReadFile(in.UnitPath); err == nil && string(have) == string(want) {
		return nil
	}
	if err := in.Change.WriteFile(in.UnitPath, want, 0o644); err != nil {
		return err
	}
	return in.run(ctx, "systemctl", "daemon-reload")
}

// unitDrift reports whether parcon.service differs from what parcon renders.
func (in *Installer) unitDrift() bool {
	have, err := os.ReadFile(in.UnitPath)
	return err != nil || string(have) != string(in.renderUnit())
}

// controllerExec runs parcon with args as the controller: in the same sandbox and with the same credentials as
// parcon.service, through systemd-run. stdin goes only to the command. timeout bounds the command in systemd too,
// so one that outlives parcon doesn't keep running.
func (in *Installer) controllerExec(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	cmd := []string{"--quiet", "--pipe", "--wait", "--collect", "--service-type=exec",
		"--description=parcon " + strings.Join(args, " "),
		"--property=RuntimeMaxSec=" + strconv.Itoa(int(timeout.Seconds()))}
	for _, p := range in.sandboxProperties() {
		cmd = append(cmd, "--property="+p)
	}
	cmd = append(cmd, "--", in.BinaryPath)
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	return in.PVE.Exec.Run(ctx, pvecli.Cmd{Name: "systemd-run", Args: append(cmd, args...)})
}

// credentialPath is where a credential is kept, encrypted.
func (in *Installer) credentialPath(name string) string { return filepath.Join(in.CredStoreDir, name) }

// hasCredential reports whether a credential is in the credential store.
func (in *Installer) hasCredential(name string) bool {
	st, err := os.Stat(in.credentialPath(name))
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// writeCredential encrypts a secret into the credential store with systemd-creds: with the host's key, and the
// TPM2 too if the node has one, unbound to PCRs so firmware and Secure Boot updates don't lock it. The secret
// travels only on systemd-creds' stdin; the command line names the credential, never its value.
func (in *Installer) writeCredential(ctx context.Context, name string, secret []byte) error {
	path := in.credentialPath(name)
	if !in.Change.DryRun {
		if err := os.MkdirAll(in.CredStoreDir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", in.CredStoreDir, err)
		}
	}
	if _, err := in.Change.Run(ctx, pvecli.Cmd{Name: "systemd-creds", Args: []string{"encrypt", "--with-key=auto",
		"--tpm2-pcrs=", "--name=" + name, "-", path}, Stdin: secret}); err != nil {
		return fmt.Errorf("encrypt the credential %s: %w", name, err)
	}
	if in.Change.DryRun {
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("encrypt the credential %s: %w", name, err)
	}
	return nil
}

// removeCredential removes a credential from the credential store.
func (in *Installer) removeCredential(name string) error {
	if _, err := os.Stat(in.credentialPath(name)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return in.Change.Remove(in.credentialPath(name))
}

// ourCredentials returns the names of the controller's credentials in the store.
func (in *Installer) ourCredentials() []string {
	entries, err := os.ReadDir(in.CredStoreDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), config.CredPrefix) && e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	return names
}

// serviceState returns parcon.service's state: active, inactive, failed, and so on.
func (in *Installer) serviceState(ctx context.Context) string {
	// is-active exits non-zero for any state but active, and prints the state either way.
	out, _ := in.PVE.Exec.Run(ctx, pvecli.C("systemctl", "is-active", ServiceName))
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return "unknown"
}

// startController enables and starts parcon.service, and returns when it started.
func (in *Installer) startController(ctx context.Context) (time.Time, error) {
	in.Out.Step("Start the controller")
	started := in.Now()
	if err := in.run(ctx, "systemctl", "enable", "--now", ServiceName); err != nil {
		return started, fmt.Errorf("starting %s failed: %w", ServiceName, err)
	}
	return started, nil
}

// restartController restarts parcon.service if it runs, and waits for its scale set sessions.
func (in *Installer) restartController(ctx context.Context, s *settings.Settings) error {
	restarted := in.Now()
	if _, err := in.Change.Run(ctx, pvecli.C("systemctl", "try-restart", ServiceName)); err != nil {
		return fmt.Errorf("restarting the controller failed: %w", err)
	}
	if in.Change.DryRun {
		return nil
	}
	if in.serviceState(ctx) != "active" {
		in.Out.Say("The controller isn't running yet; it gets the new config when parcon install starts it.")
		return nil
	}
	if err := in.waitSessions(ctx, s, restarted, 5*time.Minute); err != nil {
		return err
	}
	in.Out.Say("The controller restarted with the new config and is listening for jobs.")
	return nil
}

// waitSessions waits for the controller to log that its scale set sessions are open for every target the settings
// complete, which it does once each scale set is registered and listening. Each such line names its target, since
// the controller logs JSON with a target attribute.
func (in *Installer) waitSessions(ctx context.Context, s *settings.Settings, since time.Time,
	within time.Duration) error {
	want := map[string]bool{}
	for _, t := range s.GitHub.CompleteTargets() {
		want[t.URL] = true
	}
	for deadline := in.Now().Add(within); ; {
		out, err := in.PVE.Exec.Run(ctx, pvecli.C("journalctl", "-u", ServiceName, "--since",
			"@"+strconv.FormatInt(since.Unix(), 10), "-o", "cat"))
		missing := slices.Sorted(maps.Keys(want))
		if err == nil {
			missing = slices.DeleteFunc(missing, sessionOpened(out))
			if len(missing) == 0 {
				return nil
			}
		}
		if in.Now().After(deadline) {
			return fmt.Errorf("the controller didn't open its scale set sessions for %s within %s; see parcon status "+
				"and journalctl -u parcon", strings.Join(missing, ", "), within)
		}
		if err := in.Sleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

// sessionOpened returns whether the controller's log says a session for a target opened.
func sessionOpened(log []byte) func(target string) bool {
	opened := map[string]bool{}
	for line := range strings.Lines(string(log)) {
		var entry struct {
			Msg    string `json:"msg"`
			Target string `json:"target"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "scale set session opened" {
			opened[entry.Target] = true
		}
	}
	return func(target string) bool { return opened[target] }
}
