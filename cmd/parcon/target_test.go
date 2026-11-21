package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/installer"
)

func TestTargetUsage(t *testing.T) {
	defer func(f func() bool) { inService = f }(inService)
	inService = func() bool { return false }
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"target"}, "parcon target add --github-url URL"},
		{[]string{"target", "frobnicate"}, "parcon target list"},
		{[]string{"target", "add"}, "--github-url is required"},
		{[]string{"target", "remove"}, "parcon target remove URL"},
		{[]string{"target", "remove", "--yes"}, "parcon target remove URL"},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		err := run(tt.args, &stdout, &stderr)
		if outcomeOf(err) != usageError || !strings.Contains(stderr.String(), tt.want) {
			t.Errorf("%v: %v, stderr %q, want a usage error mentioning %q", tt.args, err, stderr.String(), tt.want)
		}
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"target", "add", "--github-url", "https://github.com/my-org", "--app", "public"}, &stdout,
		&stderr)
	if err == nil || !strings.Contains(err.Error(), `--app "public" must be manifest or manual`) {
		t.Errorf("target add --app public: %v", err)
	}
}

func TestTargetState(t *testing.T) {
	tests := []struct {
		t    installer.TargetStatus
		want string
	}{
		{installer.TargetStatus{URL: "https://github.com/a"}, "waiting for its App: parcon target add --github-url " +
			"https://github.com/a"},
		{installer.TargetStatus{SetUp: true}, "set up"},
		{installer.TargetStatus{SetUp: true, Sessions: map[string]bool{"x": true}}, "listening for jobs"},
		{installer.TargetStatus{SetUp: true, Sessions: map[string]bool{"x": true, "y": false}},
			"no session with GitHub for y"},
	}
	for _, tt := range tests {
		if got := targetState(tt.t); got != tt.want {
			t.Errorf("targetState(%+v) = %q, want %q", tt.t, got, tt.want)
		}
	}
}
