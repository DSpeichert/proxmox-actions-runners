package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/github"
)

const (
	testClientID = "Iv23liEXAMPLE0000000"
)

var testKeyPEM = func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}()

// runApp runs parcon and returns its outcome and everything it wrote or returned, which must never hold the key.
func runApp(t *testing.T, _ string, args ...string) (outcome, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(args, &stdout, &stderr)
	all := stdout.String() + stderr.String()
	if err != nil {
		all += err.Error()
	}
	if strings.Contains(all, "PRIVATE KEY") {
		t.Errorf("output leaks a secret:\n%s", all)
	}
	return outcomeOf(err), stdout.String(), all
}

func TestGitHubAppWaitInstallation(t *testing.T) {
	oldFind, oldInterval := findInstallation, installationPollInterval
	t.Cleanup(func() { findInstallation, installationPollInterval = oldFind, oldInterval })
	installationPollInterval = time.Millisecond

	dir := t.TempDir()
	key := filepath.Join(dir, "github-app.pem")
	if err := os.WriteFile(key, []byte(testKeyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	looseKey := filepath.Join(dir, "loose.pem")
	if err := os.WriteFile(looseKey, []byte(testKeyPEM), 0o644); err != nil {
		t.Fatal(err)
	}

	org := github.Installation{ID: 7890123, Account: "my-org", AccountType: "Organization"}
	user := github.Installation{ID: 7890123, Account: "octocat", AccountType: "User",
		Repositories: []string{"hello", "world"}}
	const orgOut = `{"installationId":7890123,"account":"my-org","accountType":"Organization"}` + "\n"
	tests := []struct {
		name     string
		args     []string
		found    github.Installation
		pollsTo  int   // the poll that finds the installation; 0 means never
		findErr  error // returned by every poll before it
		repoFrom int   // the poll from which the installation reaches its repositories; 0 means from the start
		want     outcome
		wantOut  string
		wantMsgs string
	}{
		{name: "installed on an organization after a few polls", found: org, pollsTo: 3, want: succeeded,
			wantOut: orgOut},
		{name: "installed on a personal account", found: user, pollsTo: 1, want: succeeded,
			wantOut: `{"installationId":7890123,"account":"octocat","accountType":"User",` +
				`"repositories":["hello","world"]}` + "\n"},
		{name: "timeout", args: []string{"-timeout", "20ms"}, want: failed,
			wantMsgs: "the GitHub App isn't installed after 20ms"},
		{name: "installed after transient errors", found: org, pollsTo: 3, findErr: errors.New("github: find the " +
			"App's installation: GET /app/installations: 502 Bad Gateway"), want: succeeded, wantOut: orgOut,
			wantMsgs: "502 Bad Gateway; still waiting"},
		{name: "bad credentials until the timeout", args: []string{"-timeout", "20ms"}, findErr: errors.New("github: " +
			"find the App's installation: GET /app/installations: 401 Unauthorized"), want: failed,
			wantMsgs: "after 20ms; last error: github: find the App's installation: GET /app/installations: " +
				"401 Unauthorized"},
		{name: "key readable by others", args: []string{"-key-file", looseKey}, want: failed,
			wantMsgs: "must not be accessible to group or others"},
		{name: "no client ID", args: []string{"-client-id", ""}, want: usageError,
			wantMsgs: "-client-id is required"},
		{name: "installed on the named account", args: []string{"-account", "my-org"}, found: org, pollsTo: 1,
			want: succeeded, wantOut: orgOut},
		{name: "several installations and no account", args: []string{"-timeout", "1s"},
			findErr: github.ErrSeveralInstallations, want: failed, wantMsgs: "pass -account"},
		{name: "the repository is added after a few polls", args: []string{"-account", "octocat", "-repository",
			"World"}, found: user, pollsTo: 1, repoFrom: 3, want: succeeded,
			wantOut: `{"installationId":7890123,"account":"octocat","accountType":"User",` +
				`"repositories":["hello","world"]}` + "\n",
			wantMsgs: "not for the repository World yet; still waiting"},
		{name: "repository without account", args: []string{"-repository", "hello"}, want: usageError,
			wantMsgs: "-repository needs -account"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			polls := 0
			wantAccount := ""
			if i := slices.Index(tt.args, "-account"); i >= 0 {
				wantAccount = tt.args[i+1]
			}
			findInstallation = func(_ context.Context, clientID, keyPEM, account string) (github.Installation,
				error) {
				polls++
				if clientID != testClientID || keyPEM != strings.TrimSpace(testKeyPEM) || account != wantAccount {
					t.Errorf("findInstallation(%q, <key>, %q) got the wrong arguments", clientID, account)
				}
				if tt.pollsTo != 0 && polls >= tt.pollsTo {
					found := tt.found
					if polls < tt.repoFrom {
						found.Repositories = []string{"hello"}
					}
					return found, nil
				}
				return github.Installation{}, tt.findErr
			}
			args := append([]string{"github", "app", "wait-installation", "-client-id", testClientID, "-key-file",
				key}, tt.args...)
			got, stdout, all := runApp(t, "", args...)
			if got != tt.want || !strings.Contains(all, tt.wantMsgs) || stdout != tt.wantOut {
				t.Errorf("outcome %s, stdout %q, want %s and %q mentioning %q:\n%s", got, stdout, tt.want, tt.wantOut,
					tt.wantMsgs, all)
			}
			if want := max(tt.pollsTo, tt.repoFrom); tt.pollsTo != 0 && polls != want {
				t.Errorf("polled %d times, want %d", polls, want)
			}
			// The same error is shown once, not on every poll.
			waited := (tt.findErr != nil || tt.repoFrom != 0) && !errors.Is(tt.findErr, github.ErrSeveralInstallations)
			if waited && strings.Count(all, "still waiting") != 1 {
				t.Errorf("the repeated error was shown %d times, want once:\n%s", strings.Count(all, "still waiting"),
					all)
			}
		})
	}
}

func TestGitHubUsage(t *testing.T) {
	for _, args := range [][]string{{"github"}, {"github", "app"}, {"github", "app", "frobnicate"},
		{"github", "app", "create"}, {"github", "app", "import"}, {"github", "x", "wait-installation"}} {
		var stdout, stderr bytes.Buffer
		err := run(args, &stdout, &stderr)
		if outcomeOf(err) != usageError || !strings.Contains(stderr.String(), "usage in the controller's sandbox") {
			t.Errorf("%v: error = %v, stderr %q", args, err, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"github", "app", "wait-installation", "-h"}, &stdout, &stderr); err != nil ||
		!strings.Contains(stderr.String(), "-key-file") {
		t.Errorf("wait-installation -h: error = %v, stderr %q", err, stderr.String())
	}
}
