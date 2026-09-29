// Package registry is Schepherd's OCI distribution transport built on ORAS.
//
// It adds the policies every network path needs: manifest and blob bodies are
// verified against the requested digest (server headers such as
// Docker-Content-Digest are never trusted), credentials are loaded lazily and
// only sent to the host they belong to, plain HTTP and extra CA bundles are
// opt-in per exact host or repository prefix, and every error is classified
// into a fault kind.
package registry

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"oras.land/oras-go/v2/registry"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/fault"
)

// Repository names an OCI repository without a tag or digest.
type Repository struct {
	// Host is the registry "host[:port]", matched verbatim against the keys
	// of Options.Hosts and the only host that receives credentials.
	Host string
	// Path is the repository path inside the registry.
	Path string
}

// ParseRepository parses "host[:port]/path". Tags, digests, upper-case
// letters and an empty path are rejected with a fault.Usage error, so the
// accepted form is canonical: String returns s unchanged.
func ParseRepository(s string) (Repository, error) {
	repo, err := parseRepository(s)
	if err != nil {
		return Repository{}, fault.Wrap(fault.Usage, err, "invalid repository %q", s)
	}

	return repo, nil
}

// String returns "host[:port]/path".
func (r Repository) String() string {
	return r.Host + "/" + r.Path
}

// Reference names one manifest: a repository plus exactly one of Tag or
// Digest.
type Reference struct {
	Repository Repository
	Tag        string
	Digest     string
}

// ParseReference parses "host[:port]/path:tag" or "host[:port]/path@sha256:…".
// A reference must carry exactly one of a tag or a digest; digests must use
// sha256. Errors are fault.Usage and quote the input.
func ParseReference(s string) (Reference, error) {
	if name, dgst, ok := strings.Cut(s, "@"); ok {
		if hasTag(name) {
			return Reference{}, fault.New(fault.Usage, "invalid reference %q: a reference must not combine a tag and a digest", s)
		}

		if err := digest.Validate(dgst); err != nil {
			return Reference{}, fault.Wrap(fault.Usage, err, "invalid reference %q", s)
		}

		repo, err := parseRepository(name)
		if err != nil {
			return Reference{}, fault.Wrap(fault.Usage, err, "invalid reference %q", s)
		}

		return Reference{Repository: repo, Digest: dgst}, nil
	}

	if !hasTag(s) {
		return Reference{}, fault.New(fault.Usage, "invalid reference %q: expected host/path:tag or host/path@sha256:<hex>", s)
	}

	colon := strings.LastIndexByte(s, ':')
	name, tag := s[:colon], s[colon+1:]

	if tag == "" {
		return Reference{}, fault.New(fault.Usage, "invalid reference %q: empty tag", s)
	}

	repo, err := parseRepository(name)
	if err != nil {
		return Reference{}, fault.Wrap(fault.Usage, err, "invalid reference %q", s)
	}

	if err := validateTag(repo, tag); err != nil {
		return Reference{}, fault.Wrap(fault.Usage, err, "invalid reference %q", s)
	}

	return Reference{Repository: repo, Tag: tag}, nil
}

// String returns "host/path@digest" or "host/path:tag".
func (r Reference) String() string {
	if r.Digest != "" {
		return r.Repository.String() + "@" + r.Digest
	}

	return r.Repository.String() + ":" + r.Tag
}

// ErrInvalidHost reports a registry host that is not "host[:port]" at all:
// a lower-case DNS name or IPv4 address, or a bracketed IPv6 address, each
// with an optional port.
var ErrInvalidHost = errors.New("invalid registry host")

// hostSyntax narrows the hosts ORAS accepts (anything a URL authority may
// hold) to names and addresses a registry can have, so a typo such as
// "r_example" fails when the configuration is loaded, not at the first
// request.
var hostSyntax = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?|\[[0-9a-f:.]+\])(?::[0-9]{1,5})?$`)

// ValidateHost accepts exactly the "host[:port]" that ParseRepository
// accepts as the host of a repository. Syntax errors wrap ErrInvalidHost;
// a port out of range and a bracketed host that is not an IPv6 address
// have messages of their own.
func ValidateHost(host string) error {
	if !hostSyntax.MatchString(host) {
		return fmt.Errorf("%w %q", ErrInvalidHost, host)
	}

	name := host
	if colon := strings.LastIndexByte(host, ':'); colon >= 0 && !strings.HasSuffix(host, "]") {
		if port, err := strconv.Atoi(host[colon+1:]); err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("host %q: the port must be between 1 and 65535", host)
		}

		name = host[:colon]
	}

	if literal, ok := strings.CutPrefix(name, "["); ok {
		if ip := strings.TrimSuffix(literal, "]"); !strings.Contains(ip, ":") || net.ParseIP(ip) == nil {
			return fmt.Errorf("host %q: %q is not an IPv6 address", host, ip)
		}
	}

	if err := (registry.Reference{Registry: host}).ValidateRegistry(); err != nil {
		return fmt.Errorf("%w %q: %w", ErrInvalidHost, host, err)
	}

	return nil
}

// ValidatePath accepts exactly the repository paths ParseRepository accepts:
// lower-case components separated by "/".
func ValidatePath(path string) error {
	if err := (registry.Reference{Repository: path}).ValidateRepository(); err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

func parseRepository(s string) (Repository, error) {
	if s != strings.ToLower(s) {
		return Repository{}, errors.New("upper-case letters are not allowed; registry hosts and repository paths must be lower case")
	}

	host, _, _ := strings.Cut(s, "/")
	if err := ValidateHost(host); err != nil {
		return Repository{}, err
	}

	ref, err := registry.ParseReference(s)
	if err != nil {
		return Repository{}, fmt.Errorf("%w", err)
	}

	if ref.Reference != "" || ref.Registry+"/"+ref.Repository != s {
		return Repository{}, errors.New("a repository must not include a tag or digest")
	}

	return Repository{Host: ref.Registry, Path: ref.Repository}, nil
}

func validateTag(repo Repository, tag string) error {
	ref := registry.Reference{Registry: repo.Host, Repository: repo.Path, Reference: tag}
	if err := ref.ValidateReferenceAsTag(); err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// hasTag reports whether the last path component of name carries ":tag".
// A colon before the first slash belongs to the registry port instead.
func hasTag(name string) bool {
	slash := strings.LastIndexByte(name, '/')

	return slash >= 0 && strings.LastIndexByte(name, ':') > slash
}
