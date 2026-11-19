package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// keyFileFor returns the -key-file flag's value, or where the App's private key lives in the controller VM by
// default: config.AppKeyFile of its Client ID.
func keyFileFor(flagValue, clientID string) string {
	if flagValue != "" {
		return flagValue
	}
	return config.AppKeyFile(clientID)
}

// maxStdin bounds what the commands read from stdin: a manifest code or a PEM key.
const maxStdin = 64 << 10

// stdin supplies the manifest code and private keys, which must never appear on a command line. Tests replace it.
var stdin io.Reader = os.Stdin

// These talk to GitHub. Tests replace them.
var (
	convertManifest  = github.ConvertManifest
	findInstallation = github.FindInstallation
	// installationPollInterval is how often wait-installation asks GitHub.
	installationPollInterval = 5 * time.Second
)

var appCommands = map[string]func(args []string, stdout, stderr io.Writer) error{
	"create":            appCreate,
	"import":            appImport,
	"wait-installation": appWaitInstallation,
}

// runGitHub handles "parcon github app <command>" and "parcon github scaleset delete". The installer runs the app
// commands to set up the GitHub App before the config names it, so they take their settings from flags and stdin,
// not from the config file.
func runGitHub(args []string, stdout, stderr io.Writer) error {
	var cmd func(args []string, stdout, stderr io.Writer) error
	switch {
	case len(args) >= 2 && args[0] == "app":
		cmd = appCommands[args[1]]
	case len(args) >= 2 && args[0] == "scaleset" && args[1] == "delete":
		cmd = scaleSetDelete
	}
	if cmd == nil {
		fmt.Fprint(stderr, vmUsage)
		return errUsage
	}
	err := cmd(args[2:], stdout, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// parseFlags parses a command's flags and rejects positional arguments. It returns flag.ErrHelp for -h.
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) error {
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return errUsage
	}
	return nil
}

// appCreate exchanges the manifest code read from stdin for the new App, writes its private key, and prints the
// App's client ID, App ID, slug, owner, and key file as JSON for the installer. The key goes to config.AppKeyFile of
// the new App's Client ID unless -key-file says otherwise.
func appCreate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("github app create", flag.ContinueOnError)
	keyFile := fs.String("key-file", "", "where to write the App's private key (default: by its Client ID in "+
		config.Dir+")")
	if err := parseFlags(fs, args, stderr); err != nil {
		return err
	}
	code, err := readStdin("manifest code")
	if err != nil {
		return err
	}
	dir := config.Dir
	if *keyFile != "" {
		dir = filepath.Dir(*keyFile)
	}
	var app github.App
	// The key's file is named by the App's Client ID, which is known only once the code is spent. The temporary file
	// is created in the directory first, so the single-use code is never spent on a directory that can't be written.
	err = writeSecretFileIn(dir, func() (string, string, error) {
		var key string
		var err error
		app, key, err = convertManifest(context.Background(), code)
		return key, keyFileFor(*keyFile, app.ClientID), err
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		ClientID  string `json:"clientId"`
		AppID     int64  `json:"appId"`
		Slug      string `json:"slug"`
		Owner     string `json:"owner"`
		OwnerType string `json:"ownerType"`
		KeyFile   string `json:"keyFile"`
	}{app.ClientID, app.ID, app.Slug, app.Owner, app.OwnerType, keyFileFor(*keyFile, app.ClientID)})
}

// appImport writes an existing App's private key, read from stdin, for installs that don't use the manifest flow.
func appImport(args []string, _, stderr io.Writer) error {
	fs := flag.NewFlagSet("github app import", flag.ContinueOnError)
	clientID := fs.String("client-id", "", "the App's client ID, which names its key file")
	keyFile := fs.String("key-file", "", "where to write the App's private key (default: by -client-id in "+
		config.Dir+")")
	if err := parseFlags(fs, args, stderr); err != nil {
		return err
	}
	if *clientID == "" && *keyFile == "" {
		fmt.Fprintln(stderr, "-client-id or -key-file is required")
		return errUsage
	}
	path := keyFileFor(*keyFile, *clientID)
	key, err := readStdin("private key")
	if err != nil {
		return err
	}
	if err := github.CheckPrivateKey(key); err != nil {
		return err
	}
	return writeSecretFile(path, func() (string, error) { return key, nil })
}

// appWaitInstallation waits until the App is installed and prints where, as JSON: the installation's ID, the
// account, its type, and, on a personal account, the repositories it can reach. The installer picks the runners'
// organization or repository from it, so the user chooses the target only once, on GitHub. -account binds the wait
// to one account, and -repository also waits until that installation reaches the repository, so the user can add
// it on GitHub while parcon waits.
func appWaitInstallation(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("github app wait-installation", flag.ContinueOnError)
	clientID := fs.String("client-id", "", "the App's client ID (required)")
	keyFile := fs.String("key-file", "", "the App's private key (default: by -client-id in "+config.Dir+")")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long to wait for the installation")
	account := fs.String("account", "", "the organization or personal account the App must be installed on")
	repository := fs.String("repository", "", "a repository the installation must reach (needs -account)")
	if err := parseFlags(fs, args, stderr); err != nil {
		return err
	}
	if *clientID == "" {
		fmt.Fprintln(stderr, "-client-id is required")
		return errUsage
	}
	if *repository != "" && *account == "" {
		fmt.Fprintln(stderr, "-repository needs -account")
		return errUsage
	}
	key, err := config.ReadSecretFile(keyFileFor(*keyFile, *clientID))
	if err != nil {
		return fmt.Errorf("GitHub App key: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	// Errors don't end the wait: a network blip, a GitHub 5xx, or a new App GitHub doesn't know yet all pass. Each
	// new error is shown right away, and the last one is repeated if the wait times out.
	var lastErr error
	reported := false
	for {
		in, err := findInstallation(ctx, *clientID, key, *account)
		if errors.Is(err, github.ErrSeveralInstallations) {
			return fmt.Errorf("%w: pass -account", err)
		}
		if in.ID != 0 && *repository != "" && !slices.ContainsFunc(in.Repositories, func(r string) bool {
			return strings.EqualFold(r, *repository)
		}) {
			if !reported {
				fmt.Fprintf(stderr, "the App is installed on %s, but not for the repository %s yet; still waiting\n",
					in.Account, *repository)
				reported = true
			}
			in = github.Installation{}
		}
		if in.ID != 0 {
			return json.NewEncoder(stdout).Encode(struct {
				InstallationID int64    `json:"installationId"`
				Account        string   `json:"account"`
				AccountType    string   `json:"accountType"`
				Repositories   []string `json:"repositories,omitempty"`
			}{in.ID, in.Account, in.AccountType, in.Repositories})
		}
		// An error after the deadline is just the deadline.
		if err != nil && ctx.Err() == nil {
			if lastErr == nil || err.Error() != lastErr.Error() {
				fmt.Fprintf(stderr, "%v; still waiting\n", err)
			}
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("the GitHub App isn't installed after %s; last error: %w", *timeout, lastErr)
			}
			return fmt.Errorf("the GitHub App isn't installed after %s", *timeout)
		case <-time.After(installationPollInterval):
		}
	}
}

// readStdin reads a secret from stdin and trims surrounding whitespace. Errors never include it.
func readStdin(what string) (string, error) {
	data, err := io.ReadAll(io.LimitReader(stdin, maxStdin))
	if err != nil {
		return "", fmt.Errorf("read the %s from stdin: %w", what, err)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", fmt.Errorf("no %s on stdin", what)
	}
	return s, nil
}

// writeSecretFile writes the secret that produce returns to path with mode 0600. It writes a temporary file in the
// same directory and renames it, so a reader never sees half a key. The temporary file is created first, so a path
// that can't be written fails before produce spends a single-use manifest code.
func writeSecretFile(path string, produce func() (string, error)) error {
	return writeSecretFileIn(filepath.Dir(path), func() (string, string, error) {
		secret, err := produce()
		return secret, path, err
	})
}

// writeSecretFileIn writes the secret produce returns to the path it returns, which must be in dir, atomically and
// readable only by its owner. The temporary file is created before produce runs, so a directory that can't be
// written fails before a single-use secret is spent.
func writeSecretFileIn(dir string, produce func() (secret, path string, err error)) (err error) {
	f, err := os.CreateTemp(dir, ".secret.*")
	if err != nil {
		return fmt.Errorf("write a file in %s: %w", dir, err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	secret, path, err := produce()
	if err != nil {
		return err
	}
	if filepath.Dir(path) != filepath.Clean(dir) {
		return fmt.Errorf("write %s: not in %s", path, dir)
	}
	// os.CreateTemp creates the file with mode 0600.
	_, err = f.WriteString(strings.TrimSpace(secret) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
