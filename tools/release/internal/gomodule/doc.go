// Package gomodule holds the local verification of the Go module release
// channel: a copy of the module, tagged in a throwaway git repository, must
// install through the go command like the public repository will, without
// network access, and the installed binary must report its tag's version.
package gomodule
