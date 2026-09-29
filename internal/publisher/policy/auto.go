package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Reasons for which automatic license detection holds a source for review.
// They are the vocabulary of Detection.Reason and Decision.AutoReason.
const (
	// RefusedNoLicense means the source has no license file to detect, or
	// none whose text could travel with the schema.
	RefusedNoLicense = "no-license"
	// RefusedNotPermissive means the detected license is not on the
	// [auto] allow list.
	RefusedNotPermissive = "not-permissive"
	// RefusedNotAsserted means the detected value names no license
	// (NOASSERTION and the like) or is not an SPDX license expression.
	RefusedNotAsserted = "not-asserted"
	// RefusedUnsupportedHost means the source is not hosted where
	// detection can pin it (a GitHub repository at a ref, an npm package at
	// an exact version) or detection may not contact the service it needs.
	RefusedUnsupportedHost = "unsupported-host"
	// RefusedFetchFailed means a request of the detection failed, a rate
	// limit included.
	RefusedFetchFailed = "fetch-failed"
)

// Services automatic license detection may contact, as listed in the hosts
// of the [auto] section.
const (
	HostGitHubAPI   = "api.github.com"
	HostNPMRegistry = "registry.npmjs.org"
	HostUnpkg       = "unpkg.com"
	HostJSDelivr    = "cdn.jsdelivr.net"
)

// MaxCombinedNoticeBytes bounds the notices of one allowed schema; a schema
// whose sources need more is held for review, leaving room below the
// artifact's notice limit for the upstream repository's own texts.
const MaxCombinedNoticeBytes = 768 << 10

const maxAllowedLicenses = 256

var (
	defaultAutoAllowed = []string{
		"MIT", "MIT-0", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "ISC", "0BSD", "CC0-1.0", "Unlicense", "BlueOak-1.0.0",
	}
	detectionHosts = []string{HostGitHubAPI, HostNPMRegistry, HostUnpkg, HostJSDelivr}
)

// Auto is the [auto] section of a policy: whether sources that no rule
// matches get their license detected, which SPDX license identifiers such a
// detection may allow, and which services it may contact.
type Auto struct {
	// Allowed are SPDX license identifiers, compared case-insensitively.
	Allowed []string
	// Hosts are the services of the Host* constants detection may contact.
	Hosts   []string
	Enabled bool
}

type autoFile struct {
	Allow   []string `toml:"allow"`
	Hosts   []string `toml:"hosts"`
	Enabled bool     `toml:"enabled"`
}

// Permits reports whether a license expression that passed
// CheckAssertedLicense lets detection allow a source: an identifier must be
// on the allow list, every operand of AND must be permitted and at least one
// operand of OR. A license with an exception (WITH) is never permitted: the
// allow list names licenses, not their variants.
func (a Auto) Permits(expr string) bool {
	if CheckAssertedLicense(expr) != nil {
		return false
	}

	e := &licenseEval{tokens: licenseTokens(expr), allowed: a.Allowed}

	return e.or() && e.pos == len(e.tokens)
}

// licenseEval evaluates an SPDX expression that passed
// CheckAssertedLicense, with SPDX precedence (WITH binds tighter than AND,
// which binds tighter than OR). Every operand is parsed even when the result
// is already known, so pos always ends after the expression.
type licenseEval struct {
	tokens  []string
	allowed []string
	pos     int
}

func (e *licenseEval) next() string {
	if e.pos >= len(e.tokens) {
		return ""
	}

	return e.tokens[e.pos]
}

func (e *licenseEval) or() bool {
	permitted := e.and()

	for e.next() == "OR" {
		e.pos++

		operand := e.and()
		permitted = permitted || operand
	}

	return permitted
}

func (e *licenseEval) and() bool {
	permitted := e.term()

	for e.next() == "AND" {
		e.pos++

		operand := e.term()
		permitted = permitted && operand
	}

	return permitted
}

func (e *licenseEval) term() bool {
	if e.next() == "(" {
		e.pos++

		permitted := e.or()
		e.pos++

		return permitted
	}

	id := e.next()
	e.pos++

	if e.next() == "WITH" {
		e.pos += 2

		return false
	}

	return !isLocalReference(id) && slices.ContainsFunc(e.allowed, func(allowed string) bool { return strings.EqualFold(allowed, id) })
}

// isLocalReference reports whether id is a LicenseRef- or DocumentRef-
// identifier. Each repository or package names such licenses itself, so the
// same identifier means different terms in different sources and an allow
// list entry cannot vouch for it.
func isLocalReference(id string) bool {
	lower := strings.ToLower(id)

	return strings.HasPrefix(lower, "licenseref-") || strings.HasPrefix(lower, "documentref-")
}

func compileAuto(f *autoFile) (Auto, error) {
	if f == nil {
		return Auto{}, nil
	}

	a := Auto{Enabled: f.Enabled, Allowed: slices.Clone(defaultAutoAllowed), Hosts: slices.Clone(detectionHosts)}

	if f.Allow != nil {
		if err := checkAllowList(f.Allow); err != nil {
			return Auto{}, err
		}

		a.Allowed = slices.Clone(f.Allow)
	}

	if f.Hosts != nil {
		if err := checkHosts(f.Hosts); err != nil {
			return Auto{}, err
		}

		a.Hosts = slices.Clone(f.Hosts)
	}

	return a, nil
}

func checkAllowList(ids []string) error {
	switch {
	case len(ids) == 0:
		return errors.New("auto.allow must not be empty; leave it out for the default list")
	case len(ids) > maxAllowedLicenses:
		return fmt.Errorf("auto.allow has more than %d entries", maxAllowedLicenses)
	}

	for i, id := range ids {
		if err := CheckAssertedLicense(id); err != nil {
			return fmt.Errorf("auto.allow: %w", err)
		}

		if len(licenseTokens(id)) != 1 {
			return fmt.Errorf("auto.allow entry %q must be a single SPDX license identifier", id)
		}

		if isLocalReference(id) {
			return fmt.Errorf("auto.allow entry %q is a LicenseRef or DocumentRef, which names a different license in every source", id)
		}

		if slices.ContainsFunc(ids[:i], func(other string) bool { return strings.EqualFold(other, id) }) {
			return fmt.Errorf("auto.allow lists %q twice", id)
		}
	}

	return nil
}

func checkHosts(hosts []string) error {
	if len(hosts) == 0 {
		return errors.New("auto.hosts must not be empty; leave it out for all supported services")
	}

	for i, host := range hosts {
		if !slices.Contains(detectionHosts, host) {
			return fmt.Errorf("auto.hosts entry %q is not one of %s", host, strings.Join(detectionHosts, ", "))
		}

		if slices.Contains(hosts[:i], host) {
			return fmt.Errorf("auto.hosts lists %q twice", host)
		}
	}

	return nil
}

// Detection records what automatic license detection found for one URL and
// what the policy made of it; the report and the license decision of
// prepared.json carry it.
type Detection struct {
	// URL is the document the detection covers.
	URL string `json:"url"`
	// Source pins where the license was read: "github:<owner>/<repo>@<commit>"
	// or "npm:<package>@<version>". Empty when detection could not pin the
	// URL.
	Source string `json:"source,omitempty"`
	// License is the detected SPDX expression, when it is one.
	License string `json:"license,omitempty"`
	// LicenseFile and NoticeFile name the files read, relative to the
	// repository or package root; the digests are of their bytes as served.
	LicenseFile   string  `json:"licenseFile,omitempty"`
	LicenseDigest string  `json:"licenseDigest,omitempty"`
	NoticeFile    string  `json:"noticeFile,omitempty"`
	NoticeDigest  string  `json:"noticeDigest,omitempty"`
	Verdict       Verdict `json:"verdict"`
	// Reason is one of the Refused* codes when Verdict is Review.
	Reason string `json:"reason,omitempty"`
}

// Finding is the answer of automatic license detection for one URL. Either
// Failure is the Refused* code of a problem detection ran into, explained by
// Detail, or License is the detected value and Notice the text the schema's
// notice layer must carry for it.
type Finding struct {
	Source        string
	License       string
	LicenseFile   string
	LicenseDigest string
	NoticeFile    string
	NoticeDigest  string
	Notice        string
	Failure       string
	Detail        string
}

// Lookup returns the detection finding for a URL no rule matches; false
// means detection has no answer for it and the URL stays unruled.
type Lookup func(url string) (Finding, bool)

// Auto returns the policy's [auto] section; it is disabled when the file has
// none.
func (p *Policy) Auto() Auto {
	return Auto{Enabled: p.auto.Enabled, Allowed: slices.Clone(p.auto.Allowed), Hosts: slices.Clone(p.auto.Hosts)}
}

// HasRule reports whether a rule decides url. Only URLs without one are
// left to automatic detection: explicit rules always take precedence.
func (p *Policy) HasRule(url string) bool {
	return p.evaluate(url).rule != nil
}

// ValidRuleID reports whether id is a valid rule ID.
func ValidRuleID(id string) bool {
	return ruleIDPattern.MatchString(id)
}

// RuleID returns the ID of the rule that decides url, or "" when none does.
func (p *Policy) RuleID(url string) string {
	if r := p.evaluate(url).rule; r != nil {
		return r.id
	}

	return ""
}

// Declared returns the license a local source file declares for url.
type Declared func(url string) (license string, ok bool)

// DecideWith is Decide with automatic license detection and the license
// declarations of a local source file.
//
// When the policy's [auto] section is enabled, every URL that no rule
// decides and for which lookup has a finding is allowed when the finding's
// license is permitted (see Auto.Permits) and held for review otherwise.
// Rules keep precedence, so an exclude or review rule holds a schema
// whatever detection found. The decision lists every such URL in Detections.
//
// A declaration is the maintainer's reviewed decision for exactly that URL:
// it takes the place of an allow rule, of detection (which is not consulted
// for it) and of a missing rule, but an exclude or review rule still wins
// over it. A declaration that asserts no license (see CheckAssertedLicense)
// holds the schema for review. lookup and declared may be nil; a nil policy
// has no rules and no [auto] section.
func (p *Policy) DecideWith(sourceURL string, dependencyURLs []string, lookup Lookup, declared Declared) Decision {
	if p == nil || !p.auto.Enabled {
		lookup = nil
	}

	judge := func(o outcome) outcome { return p.detected(declare(o, declared), lookup) }

	outcomes := make([]outcome, 0, 1+len(dependencyURLs))
	outcomes = append(outcomes, judge(p.evaluate(sourceURL)))

	for _, o := range p.dependencies(sourceURL, dependencyURLs) {
		outcomes = append(outcomes, judge(o))
	}

	for _, verdict := range []Verdict{Exclude, Review} {
		if d, ok := blocked(outcomes, verdict); ok {
			return d
		}
	}

	return allowed(outcomes)
}

func declare(o outcome, declared Declared) outcome {
	if declared == nil || (o.rule != nil && o.rule.verdict != Allow) {
		return o
	}

	license, ok := declared(o.url)
	if !ok {
		return o
	}

	return outcome{url: o.url, declared: &declaration{license: license, err: CheckAssertedLicense(license)}}
}

func (p *Policy) detected(o outcome, lookup Lookup) outcome {
	if o.rule != nil || o.declared != nil || o.problem != "" || lookup == nil {
		return o
	}

	if f, ok := lookup(o.url); ok {
		o.auto = p.judge(o.url, &f)
	}

	return o
}

// autoOutcome is the policy's verdict on a finding; it plays the part of a
// rule in the combination of a schema's outcomes.
type autoOutcome struct {
	detection Detection
	license   string
	notice    string
	reason    string
}

func (p *Policy) judge(url string, f *Finding) *autoOutcome {
	d := Detection{
		URL: url, Source: f.Source, LicenseFile: f.LicenseFile, LicenseDigest: f.LicenseDigest,
		NoticeFile: f.NoticeFile, NoticeDigest: f.NoticeDigest, Verdict: Review,
	}
	if CheckLicense(f.License) == nil {
		d.License = f.License
	}

	refuse := func(reason, detail string) *autoOutcome {
		d.Reason = reason

		return &autoOutcome{detection: d, reason: NoRuleReason + "; automatic license detection (" + reason + "): " + detail}
	}

	switch {
	case f.Failure != "":
		return refuse(f.Failure, f.Detail)
	case f.License == "":
		return refuse(RefusedNoLicense, "no license detected in "+f.Source)
	}

	if err := CheckAssertedLicense(f.License); err != nil {
		return refuse(RefusedNotAsserted, err.Error()+" ("+f.Source+")")
	}

	if !p.auto.Permits(f.License) {
		return refuse(RefusedNotPermissive, fmt.Sprintf("license %q of %s is not on the [auto] allow list", f.License, f.Source))
	}

	if strings.TrimSpace(f.Notice) == "" {
		return refuse(RefusedNoLicense, "no license text of "+f.Source+" to carry with the schema")
	}

	d.Verdict = Allow

	return &autoOutcome{
		detection: d, license: f.License, notice: f.Notice,
		reason: "license " + f.License + " detected in " + f.Source + " (automatic)",
	}
}
