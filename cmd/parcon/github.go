package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// keyFileFor returns the -key-file flag's value, or by default the App's private key among the service's
// credentials.
func keyFileFor(flagValue, clientID string) string {
	if flagValue != "" {
		return flagValue
	}
	return config.CredentialFile(config.CredAppKey(clientID))
}

// These talk to GitHub. Tests replace them.
var (
	findInstallation = github.FindInstallation
	// installationPollInterval is how often wait-installation asks GitHub.
	installationPollInterval = 5 * time.Second
)

var appCommands = map[string]func(args []string, stdout, stderr io.Writer) error{
	"wait-installation": appWaitInstallation,
}

// runGitHub handles "parcon github app wait-installation" and "parcon github scaleset delete", which the host runs in
// the controller's sandbox, with its credentials. wait-installation runs before the config names the App, so it takes
// its settings from flags, not from the config file.
func runGitHub(args []string, stdout, stderr io.Writer) error {
	var cmd func(args []string, stdout, stderr io.Writer) error
	switch {
	case len(args) >= 2 && args[0] == "app":
		cmd = appCommands[args[1]]
	case len(args) >= 2 && args[0] == "scaleset" && args[1] == "delete":
		cmd = scaleSetDelete
	}
	if cmd == nil {
		fmt.Fprint(stderr, serviceUsage)
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

// appWaitInstallation waits until the App is installed and prints where, as JSON: the installation's ID, the
// account, its type, and, on a personal account, the repositories it can reach. The installer picks the runners'
// organization or repository from it, so the user chooses the target only once, on GitHub. -account binds the wait
// to one account, and -repository also waits until that installation reaches the repository, so the user can add
// it on GitHub while parcon waits.
func appWaitInstallation(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("github app wait-installation", flag.ContinueOnError)
	clientID := fs.String("client-id", "", "the App's client ID (required)")
	keyFile := fs.String("key-file", "", "the App's private key (default: its credential, by -client-id)")
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
