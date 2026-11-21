package release

import (
	"fmt"
	"os"

	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
)

// ReplaceBinary installs the executable at src as dst atomically, so a parcon running from dst keeps running and
// the next start gets the new one.
func ReplaceBinary(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: the parcon the user runs.
	if err != nil {
		return fmt.Errorf("install %s: %w", dst, err)
	}
	return pvecli.WriteFileAtomic(dst, data, 0o755)
}
