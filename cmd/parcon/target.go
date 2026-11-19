package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/klponce/proxmox-actions-runners/internal/installer"
)

const targetUsage = `usage:
  parcon target list [--json]                  list the organizations and repositories the runners serve
  parcon target add --github-url URL [flags]   serve another organization or repository
      --app manifest|manual                    for an account without an App yet: create one in a browser
                                               (default), or use an existing App with --client-id
      --client-id ID                           the existing App's Client ID, with --app manual
      --yes, --dry-run
  parcon target remove URL [--yes] [--dry-run] stop serving one; its scale sets are deleted in GitHub
`

// runTarget handles "parcon target" on the host.
func runTarget(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, targetUsage)
		return errUsage
	}
	switch args[0] {
	case "list":
		return targetList(args[1:], stdout, stderr)
	case "add":
		return targetAdd(args[1:], stdout, stderr)
	case "remove":
		return targetRemove(args[1:], stdout, stderr)
	default:
		fmt.Fprint(stderr, targetUsage)
		return errUsage
	}
}

func targetAdd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("target add", flag.ContinueOnError)
	var h hostFlags
	h.register(fs, false)
	url := fs.String("github-url", "", "the organization or repository, as https://github.com/<org> or "+
		"https://github.com/<owner>/<repo>")
	app := fs.String("app", string(installer.AppManifest), "for an account without an App yet: manifest (create "+
		"one in a browser) or manual (use an existing App)")
	clientID := fs.String("client-id", "", "the existing App's Client ID, with --app manual")
	if err := parseHostFlags(fs, args, stderr); err != nil {
		return usageOrNil(err)
	}
	if *url == "" {
		fmt.Fprintf(stderr, "--github-url is required\n%s", targetUsage)
		return errUsage
	}
	if *app != string(installer.AppManifest) && *app != string(installer.AppManual) {
		return fmt.Errorf("--app %q must be manifest or manual", *app)
	}
	in, err := newInstaller(stdout, stderr, h.dryRun)
	if err != nil {
		return err
	}
	in.Yes = h.yes
	ctx, cancel := hostContext()
	defer cancel()
	return in.AddTarget(ctx, installer.TargetOptions{URL: *url, App: installer.AppMode(*app), ClientID: *clientID})
}

func targetRemove(args []string, stdout, stderr io.Writer) error {
	// The URL comes first, like the other verbs' arguments, and the flags after it.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(stderr, targetUsage)
		return errUsage
	}
	url, args := args[0], args[1:]
	fs := flag.NewFlagSet("target remove", flag.ContinueOnError)
	var h hostFlags
	h.register(fs, false)
	if err := parseHostFlags(fs, args, stderr); err != nil {
		return usageOrNil(err)
	}
	in, err := newInstaller(stdout, stderr, h.dryRun)
	if err != nil {
		return err
	}
	in.Yes = h.yes
	ctx, cancel := hostContext()
	defer cancel()
	return in.RemoveTarget(ctx, url)
}

func targetList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("target list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the targets as JSON")
	if err := parseHostFlags(fs, args, stderr); err != nil {
		return usageOrNil(err)
	}
	in, err := newInstaller(stdout, stderr, false)
	if err != nil {
		return err
	}
	ctx, cancel := hostContext()
	defer cancel()
	targets, err := in.Targets(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(targets)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TARGET\tAPP\tINSTALLATION\tSTATE")
	for _, t := range targets {
		app, inst := t.App, fmt.Sprint(t.InstallationID)
		if app == "" {
			app = "-"
		}
		if t.InstallationID == 0 {
			inst = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.URL, app, inst, targetState(t))
	}
	return w.Flush()
}

// targetState says whether a target is set up and listening for jobs.
func targetState(t installer.TargetStatus) string {
	switch {
	case !t.SetUp:
		return "waiting for its App: parcon target add --github-url " + t.URL
	case t.Sessions == nil:
		return "set up"
	}
	var closed []string
	for _, name := range slices.Sorted(maps.Keys(t.Sessions)) {
		if !t.Sessions[name] {
			closed = append(closed, name)
		}
	}
	if len(closed) > 0 {
		return "no session with GitHub for " + strings.Join(closed, ", ")
	}
	return "listening for jobs"
}
