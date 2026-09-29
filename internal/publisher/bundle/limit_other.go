//go:build !linux

package bundle

// limitResources does nothing where one process cannot lower another's
// resource limits (prlimit is Linux-only); the per-run timeout still bounds
// every CLI run.
func limitResources(int, uint64) error {
	return nil
}
