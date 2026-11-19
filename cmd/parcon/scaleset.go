package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// scaleSetClient is the part of the GitHub client that scaleset delete uses.
type scaleSetClient interface {
	FindScaleSet(ctx context.Context, spec github.ScaleSetSpec) (*github.ScaleSet, error)
	DeleteScaleSet(ctx context.Context, id int) error
}

// newScaleSetClients builds the GitHub clients for scaleset delete, by target key. Tests replace it.
var newScaleSetClients = func(cfg *config.Config) (map[string]scaleSetClient, error) {
	clients, err := newGitHubClients(cfg, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]scaleSetClient{}
	for k, c := range clients {
		out[k] = c
	}
	return out, nil
}

// scaleSetDelete removes the configured scale sets from GitHub, which unregisters their runners: for every target,
// or with -target, for one. The installer runs it on uninstall, after it stops the controller, which would
// otherwise register them again, and before it removes a target. A failing target doesn't stop the others.
func scaleSetDelete(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("github scaleset delete", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath, "path of the config file")
	only := fs.String("target", "", "the organization or repository URL to delete the scale sets of; empty for all")
	if err := parseFlags(fs, args, stderr); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	targets := cfg.GitHub.Targets
	if *only != "" {
		key := config.TargetKey(*only)
		targets = slices.DeleteFunc(slices.Clone(targets), func(t config.Target) bool { return t.Key() != key })
		if key == "" || len(targets) == 0 {
			return fmt.Errorf("%s isn't a target in %s", *only, *path)
		}
	}
	clients, err := newScaleSetClients(cfg)
	if err != nil {
		return fmt.Errorf("GitHub App: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	var errs []error
	for _, t := range targets {
		client := clients[t.Key()]
		for _, s := range cfg.ScaleSets {
			if err := deleteScaleSet(ctx, client, t, s, stdout); err != nil {
				errs = append(errs, fmt.Errorf("%s: scale set %s: %w", t.ConfigURL, s.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func deleteScaleSet(ctx context.Context, client scaleSetClient, t config.Target, s config.ScaleSet,
	stdout io.Writer) error {
	found, err := client.FindScaleSet(ctx, github.ScaleSetSpec{Name: s.Name, Labels: s.Labels,
		RunnerGroup: s.RunnerGroupFor(t)})
	if err != nil {
		return err
	}
	if found == nil {
		fmt.Fprintf(stdout, "%s: scale set %s: not registered\n", t.ConfigURL, s.Name)
		return nil
	}
	if err := client.DeleteScaleSet(ctx, found.ID); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: scale set %s: deleted (ID %d)\n", t.ConfigURL, s.Name, found.ID)
	return nil
}
