package main

import (
	"os"
	"testing"
)

// The tests run the controller's commands by default, as in its sandbox; the host commands' tests say otherwise.
func TestMain(m *testing.M) {
	inService = func() bool { return true }
	os.Exit(m.Run())
}
