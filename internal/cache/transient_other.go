//go:build !windows

package cache

// transient reports errors that clear when retried shortly. Only Windows has
// them: elsewhere a rename or open does not depend on other open handles.
func transient(error) bool { return false }
