// Package fault defines the error classes Schepherd reports for its own
// operations and the process exit code attached to each class.
//
// Exit codes returned by an external consumer launched through the runner are
// passed through unchanged via ConsumerExitError and may therefore collide with the
// codes below; diagnostics on stderr name the source of every failure.
package fault

import (
	"context"
	"errors"
	"fmt"
)

// Kind classifies a failure. Each kind maps to exactly one exit code.
type Kind int

const (
	// Internal is an unexpected failure inside Schepherd (exit 1).
	Internal Kind = iota
	// Usage covers invalid CLI arguments, invalid configuration and
	// unsupported format versions (exit 2).
	Usage
	// NotFound covers unknown schema IDs, unmatched files and ambiguous
	// matches (exit 3).
	NotFound
	// Registry covers registry, authentication and network failures (exit 4).
	Registry
	// Integrity covers digest or size mismatches, invalid artifacts and
	// exceeded resource limits (exit 5).
	Integrity
	// Offline covers cache misses and corrupt cache entries while running
	// with --offline (exit 6).
	Offline
	// ConsumerStart means the configured consumer could not be started
	// (exit 7).
	ConsumerStart
	// ConsumerTimeout means a consumer exceeded runner.timeout (exit 124).
	ConsumerTimeout
	// Canceled means the operation was interrupted by the user (exit 130).
	Canceled
)

var exitCodes = map[Kind]int{
	Internal:        1,
	Usage:           2,
	NotFound:        3,
	Registry:        4,
	Integrity:       5,
	Offline:         6,
	ConsumerStart:   7,
	ConsumerTimeout: 124,
	Canceled:        130,
}

var kindNames = map[Kind]string{
	Internal:        "internal",
	Usage:           "usage",
	NotFound:        "not-found",
	Registry:        "registry",
	Integrity:       "integrity",
	Offline:         "offline",
	ConsumerStart:   "consumer-start",
	ConsumerTimeout: "consumer-timeout",
	Canceled:        "canceled",
}

// ExitCode returns the process exit code for the kind.
func (k Kind) ExitCode() int {
	if code, ok := exitCodes[k]; ok {
		return code
	}

	return 1
}

// String returns the stable machine-readable name of the kind.
func (k Kind) String() string {
	if name, ok := kindNames[k]; ok {
		return name
	}

	return "internal"
}

// Error is a classified failure with a human-readable message.
type Error struct {
	Err  error
	Msg  string
	Kind Kind
}

// Error implements the error interface.
func (e *Error) Error() string {
	switch {
	case e.Msg == "" && e.Err != nil:
		return e.Err.Error()
	case e.Err != nil:
		return e.Msg + ": " + e.Err.Error()
	default:
		return e.Msg
	}
}

// Unwrap exposes the wrapped cause.
func (e *Error) Unwrap() error {
	return e.Err
}

// New returns a classified error with a formatted message.
func New(kind Kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// Wrap classifies err. An err that is already classified keeps its original
// kind unless it is nil, so wrapping never downgrades a more specific class.
func Wrap(kind Kind, err error, format string, args ...any) *Error {
	if existing, ok := errors.AsType[*Error](err); ok {
		kind = existing.Kind
	}

	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...), Err: err}
}

// Reclassify wraps err with an explicit kind even if err is already classified.
func Reclassify(kind Kind, err error, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...), Err: err}
}

// ConsumerExitError carries the exit status of an external consumer process that
// Schepherd must propagate unchanged.
type ConsumerExitError struct {
	Code int
}

// Error implements the error interface.
func (e *ConsumerExitError) Error() string {
	return fmt.Sprintf("consumer exited with status %d", e.Code)
}

// KindOf returns the kind of the outermost classified error in the chain.
// Context cancellation is reported as Canceled.
func KindOf(err error) Kind {
	if classified, ok := errors.AsType[*Error](err); ok {
		return classified.Kind
	}

	if errors.Is(err, context.Canceled) {
		return Canceled
	}

	return Internal
}

// ExitCodeOf returns the exit code for err; nil maps to 0.
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}

	if consumer, ok := errors.AsType[*ConsumerExitError](err); ok {
		return consumer.Code
	}

	return KindOf(err).ExitCode()
}
