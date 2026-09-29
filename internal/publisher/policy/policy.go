// Package policy decides whether a schema source may be published, under
// which license and with which notices. Rules come from sources/licenses.toml
// and are written by maintainers after reviewing a source's license.
//
// Nothing is allowed by default: a source (or dependency) that no rule matches
// is held for review. A schema is allowed only when its own source and every
// dependency it embeds are allowed.
//
// URLs are compared after normalization (http and https treated alike,
// lowercase host, default port, fragment and percent-encoding variants
// removed, and on github.com and raw.githubusercontent.com the owner and
// repository in any letter case, as GitHub resolves them). The policy does not know which URLs serve the same document, so
// exact URLs and path prefixes only cover the spellings they name. A
// restrictive rule that must survive aliases (another host, an extensionless
// path, another directory) names the document by file_names instead, which
// match the last path segment on the rule's hosts.
package policy

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/upstream/tomlfile"
)

// Verdict is the outcome of a decision.
type Verdict string

const (
	// Allow means the schema may be published.
	Allow Verdict = "allow"
	// Exclude means a maintainer decided the source must not be published.
	Exclude Verdict = "exclude"
	// Review means no decision exists yet; the source is not published.
	Review Verdict = "review"
)

// NoRuleReason is the reason given for a source no rule matches.
const NoRuleReason = "no license rule matched"

// DeclaredReason is the reason given for a license a local source file
// declares.
const DeclaredReason = "license declared in the local source file"

const (
	maxPolicyBytes  = 1 << 20
	maxNoticeBytes  = 64 << 10
	maxLicenseBytes = 256
	maxReasonBytes  = 4096
	maxURLBytes     = 4096
	maxRules        = 10000
	maxFileName     = 255
)

var (
	ruleIDPattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	hostPattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	licensePattern = regexp.MustCompile(`^[A-Za-z0-9.+:()-]+(?: [A-Za-z0-9.+:()-]+)*$`)
)

// Decision is the result of Decide and DecideWith.
type Decision struct {
	// Decision is the combined verdict for the source and its dependencies.
	Decision Verdict
	// License is set for Allow: the distinct licenses of all involved rules,
	// detections and declarations, sorted and joined with " AND "
	// (expressions containing " OR " are parenthesized).
	License string
	// Notice is set for Allow: the distinct notices of all involved rules
	// and detections, the source's first, then by rule ID (a detection by
	// its source), separated by blank lines.
	Notice string
	// Reason explains the verdict for the report: the distinct reasons in
	// the order of the notices.
	Reason string
	// RuleID is the rule that decided the verdict: the source's rule for
	// Allow, otherwise the first rule that excluded or held the schema for
	// review. It is empty when the deciding URL matched no rule.
	RuleID string
	// AutoReason is the Refused* code of automatic license detection when
	// it decided the verdict: the first URL that held the schema for review
	// was decided by detection.
	AutoReason string
	// Detections lists every URL of the schema that automatic license
	// detection decided, the source first, then the dependencies by URL.
	Detections []Detection
}

// Policy is a loaded rule set.
type Policy struct {
	exact map[string]*rule
	hosts map[string][]*rule
	auto  Auto
}

type rule struct {
	id         string
	verdict    Verdict
	license    string
	notice     string
	reason     string
	pathPrefix string
	fileStems  []string
}

type fileRule struct {
	ID         string   `toml:"id"`
	Decision   string   `toml:"decision"`
	License    string   `toml:"license"`
	Notice     string   `toml:"notice"`
	NoticeFile string   `toml:"notice_file"`
	Reason     string   `toml:"reason"`
	PathPrefix string   `toml:"path_prefix"`
	URLs       []string `toml:"urls"`
	Hosts      []string `toml:"hosts"`
	FileNames  []string `toml:"file_names"`
}

type file struct {
	Auto  *autoFile  `toml:"auto"`
	Rules []fileRule `toml:"rules"`
}

// Load reads a policy file. Each [[rules]] entry has an id, a decision
// (allow, exclude or review), a reason, and either an exact list of urls or a
// list of hosts with an optional path_prefix (matched at "/" boundaries).
// Exclude and review rules with hosts may also list file_names: such a rule
// matches every URL on its hosts and under its path_prefix whose last path
// segment is one of the names, compared case-insensitively and with or
// without a ".json" extension, whatever the query. Allow rules need a license
// and cannot use file_names, so an alias never widens what is allowed. A
// notice is given inline (notice) or as a file relative to the policy file
// (notice_file).
//
// Precedence: exact URLs, then file_names rules, then the other host rules;
// among rules of one kind the longest matching path_prefix wins. Host rules
// without file_names never match a URL with a query, which can select a
// document the rule was not written for.
//
// An optional [auto] section enables automatic license detection for URLs
// no rule matches (see DecideWith):
//
//	[auto]
//	enabled = true
//	allow = ["MIT", "Apache-2.0"]   # optional; SPDX identifiers
//	hosts = ["api.github.com"]      # optional; services detection may use
//
// allow defaults to MIT, MIT-0, Apache-2.0, BSD-2-Clause, BSD-3-Clause, ISC,
// 0BSD, CC0-1.0, Unlicense and BlueOak-1.0.0; hosts to every Host* service.
// Configuration errors are fault.Usage.
func Load(path string) (*Policy, error) {
	var doc file
	if err := tomlfile.Decode(path, maxPolicyBytes, &doc); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "license policy")
	}

	p, err := build(doc, filepath.Dir(path))
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "license policy %s", path)
	}

	return p, nil
}

// Decide combines the rules for sourceURL and its dependencyURLs.
// Dependencies are deduplicated after normalization; official JSON Schema
// metaschemas (see IsWellKnownMetaschema) and the source itself are not
// dependencies and are skipped. Exclude wins over Review, which wins over
// Allow. Decide never consults automatic license detection.
func (p *Policy) Decide(sourceURL string, dependencyURLs []string) Decision {
	return p.DecideWith(sourceURL, dependencyURLs, nil, nil)
}

func (p *Policy) add(r *rule, fr *fileRule) error {
	for _, raw := range fr.URLs {
		key, err := normalize(raw)
		if err != nil {
			return fmt.Errorf("url %q: %w", raw, err)
		}

		if key.fragment {
			return fmt.Errorf("url %q must not contain a fragment", raw)
		}

		if schemaStoreAlias(key) {
			return fmt.Errorf("url %q is another spelling of a SchemaStore URL; rules are compared with the canonical "+
				"https://www.schemastore.org/<file>.json only, so write that form or use file_names on the SchemaStore hosts", raw)
		}

		if other, ok := p.exact[key.exact]; ok {
			return fmt.Errorf("url %q is already covered by rule %q", raw, other.id)
		}

		p.exact[key.exact] = r
	}

	for _, host := range fr.Hosts {
		for _, other := range p.hosts[host] {
			if foldGitHubRepository(host, other.pathPrefix) == foldGitHubRepository(host, r.pathPrefix) &&
				overlaps(r.fileStems, other.fileStems) {
				return fmt.Errorf("host %q with path_prefix %q is already covered by rule %q", host, r.pathPrefix, other.id)
			}
		}

		p.hosts[host] = append(p.hosts[host], r)
	}

	return nil
}

// rank orders the host rules of one host: file_names rules are checked
// first, because they carve single documents out of a host.
func (r *rule) rank() int {
	if len(r.fileStems) > 0 {
		return 0
	}

	return 1
}

// overlaps reports whether two host rules with the same path_prefix can match
// the same URL.
func overlaps(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}

	return slices.ContainsFunc(a, func(stem string) bool { return slices.Contains(b, stem) })
}

// evaluate finds the rule for raw; a nil policy has no rules.
func (p *Policy) evaluate(raw string) outcome {
	if p == nil {
		return outcome{url: raw}
	}

	key, err := normalize(raw)
	if err != nil {
		return outcome{url: raw, problem: "unsupported URL: " + err.Error()}
	}

	if r, ok := p.exact[key.exact]; ok {
		return outcome{url: raw, rule: r}
	}

	if !key.portless {
		return outcome{url: raw}
	}

	for _, r := range p.hosts[key.host] {
		if !prefixMatches(key.path, foldGitHubRepository(key.host, r.pathPrefix)) {
			continue
		}

		switch {
		case len(r.fileStems) > 0:
			if slices.Contains(r.fileStems, key.stem) {
				return outcome{url: raw, rule: r}
			}
		case !key.query:
			return outcome{url: raw, rule: r}
		}
	}

	return outcome{url: raw}
}

func (p *Policy) dependencies(sourceURL string, dependencyURLs []string) []outcome {
	seen := map[string]bool{}
	if key, err := normalize(sourceURL); err == nil {
		seen[key.exact] = true
	}

	var raws []string

	for _, raw := range dependencyURLs {
		if IsWellKnownMetaschema(raw) {
			continue
		}

		key := raw
		if n, err := normalize(raw); err == nil {
			key = n.exact
		}

		if !seen[key] {
			seen[key] = true

			raws = append(raws, raw)
		}
	}

	slices.Sort(raws)

	out := make([]outcome, 0, len(raws))
	for _, raw := range raws {
		out = append(out, p.evaluate(raw))
	}

	return out
}

func build(doc file, baseDir string) (*Policy, error) {
	if len(doc.Rules) > maxRules {
		return nil, fmt.Errorf("%d rules exceed the limit of %d", len(doc.Rules), maxRules)
	}

	auto, err := compileAuto(doc.Auto)
	if err != nil {
		return nil, err
	}

	p := &Policy{exact: map[string]*rule{}, hosts: map[string][]*rule{}, auto: auto}
	ids := map[string]bool{}

	for i := range doc.Rules {
		fr := &doc.Rules[i]

		r, err := compileRule(fr, baseDir)
		if err != nil {
			return nil, fmt.Errorf("rule %d (%q): %w", i+1, fr.ID, err)
		}

		if ids[r.id] {
			return nil, fmt.Errorf("duplicate rule id %q", r.id)
		}

		ids[r.id] = true

		if err := p.add(r, fr); err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.id, err)
		}
	}

	for host := range p.hosts {
		slices.SortStableFunc(p.hosts[host], func(a, b *rule) int {
			return cmp.Or(cmp.Compare(a.rank(), b.rank()), len(b.pathPrefix)-len(a.pathPrefix))
		})
	}

	return p, nil
}

func compileRule(fr *fileRule, baseDir string) (*rule, error) {
	r := &rule{id: fr.ID, verdict: Verdict(fr.Decision), license: fr.License, reason: fr.Reason}

	if !ruleIDPattern.MatchString(fr.ID) {
		return nil, fmt.Errorf("id must match %s", ruleIDPattern)
	}

	switch r.verdict {
	case Allow, Exclude, Review:
	default:
		return nil, fmt.Errorf("decision %q must be allow, exclude or review", fr.Decision)
	}

	if err := checkText("reason", fr.Reason, maxReasonBytes, true); err != nil {
		return nil, err
	}

	switch {
	case r.verdict == Allow:
		if err := CheckAssertedLicense(fr.License); err != nil {
			return nil, err
		}
	case fr.License != "":
		if err := CheckLicense(fr.License); err != nil {
			return nil, err
		}
	}

	if err := checkMatchers(fr); err != nil {
		return nil, err
	}

	stems, err := fileStems(fr)
	if err != nil {
		return nil, err
	}

	r.pathPrefix, r.fileStems = fr.PathPrefix, stems

	notice, err := loadNotice(fr, baseDir)
	if err != nil {
		return nil, err
	}

	r.notice = notice

	return r, nil
}

func checkMatchers(fr *fileRule) error {
	switch {
	case len(fr.URLs) > 0 && (len(fr.Hosts) > 0 || fr.PathPrefix != "" || len(fr.FileNames) > 0):
		return errors.New("urls cannot be combined with hosts, path_prefix or file_names")
	case len(fr.URLs) == 0 && len(fr.Hosts) == 0:
		return errors.New("either urls or hosts is required")
	}

	seen := map[string]bool{}

	for _, host := range fr.Hosts {
		if !hostPattern.MatchString(host) || strings.Contains(host, "..") {
			return fmt.Errorf("host %q must be a lowercase host name without scheme, port or path", host)
		}

		if seen[host] {
			return fmt.Errorf("host %q is listed twice", host)
		}

		seen[host] = true
	}

	if fr.PathPrefix != "" {
		if !strings.HasPrefix(fr.PathPrefix, "/") || strings.ContainsAny(fr.PathPrefix, "%?#\\") ||
			hasDotSegment(fr.PathPrefix) || !utf8.ValidString(fr.PathPrefix) {
			return fmt.Errorf("path_prefix %q must be an absolute, unescaped path without dot segments", fr.PathPrefix)
		}
	}

	return nil
}

// fileStems validates file_names and returns them lowercased without a
// ".json" extension, the form URLs are compared in.
func fileStems(fr *fileRule) ([]string, error) {
	if len(fr.FileNames) == 0 {
		return nil, nil
	}

	if Verdict(fr.Decision) == Allow {
		return nil, errors.New("file_names is only allowed for exclude and review rules")
	}

	stems := make([]string, 0, len(fr.FileNames))

	for _, name := range fr.FileNames {
		stem := fileStem(name)

		switch {
		case len(name) > maxFileName || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\%?#") ||
			strings.ContainsFunc(name, unicode.IsControl) || name == "." || name == "..":
			return nil, fmt.Errorf("file_names entry %q must be a plain, unescaped file name of at most %d bytes", name, maxFileName)
		case stem == "":
			return nil, fmt.Errorf("file_names entry %q has no name before its extension", name)
		case slices.Contains(stems, stem):
			return nil, fmt.Errorf("file_names entry %q is listed twice", name)
		}

		stems = append(stems, stem)
	}

	return stems, nil
}

func fileStem(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".json")
}

func loadNotice(fr *fileRule, baseDir string) (string, error) {
	if fr.Notice != "" && fr.NoticeFile != "" {
		return "", errors.New("notice and notice_file are mutually exclusive")
	}

	notice := fr.Notice

	if fr.NoticeFile != "" {
		name := filepath.FromSlash(fr.NoticeFile)
		if strings.Contains(fr.NoticeFile, "\\") || !filepath.IsLocal(name) {
			return "", fmt.Errorf("notice_file %q must be a relative path inside the policy directory", fr.NoticeFile)
		}

		data, err := readInside(baseDir, name)
		if err != nil {
			return "", fmt.Errorf("notice_file %q: %w", fr.NoticeFile, err)
		}

		notice = string(data)
	}

	notice = strings.TrimRight(notice, "\r\n")

	if err := checkText("notice", notice, maxNoticeBytes, false); err != nil {
		return "", err
	}

	return notice, nil
}

func readInside(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open policy directory: %w", err)
	}

	defer func() { _ = root.Close() }()

	info, err := root.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}

	if !info.Mode().IsRegular() || info.Size() > maxNoticeBytes {
		return nil, fmt.Errorf("must be a regular file of at most %d bytes", maxNoticeBytes)
	}

	data, err := root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	if len(data) > maxNoticeBytes {
		return nil, fmt.Errorf("must be at most %d bytes", maxNoticeBytes)
	}

	return data, nil
}

// CheckLicense validates an SPDX-like license expression: identifiers made of
// letters, digits and ".+:-" (LicenseRef-… included), parentheses and single
// spaces, at most 256 bytes. It does not check identifiers against the SPDX
// list.
func CheckLicense(expr string) error {
	if expr == "" {
		return errors.New("license is required")
	}

	if len(expr) > maxLicenseBytes || !licensePattern.MatchString(expr) {
		return fmt.Errorf("license %q must be an SPDX-like expression of at most %d bytes", expr, maxLicenseBytes)
	}

	return nil
}

// nonAssertions are license values that name no license: SPDX's NOASSERTION
// and NONE, npm's UNLICENSED and the placeholders UNKNOWN and OTHER. None of
// them grants permission to redistribute.
var nonAssertions = []string{"NOASSERTION", "NONE", "OTHER", "UNKNOWN", "UNLICENSED"}

// CheckAssertedLicense validates a license that is to allow publication. It
// must pass CheckLicense, be an SPDX license expression (identifiers joined
// by AND, OR and WITH, grouped by parentheses; so npm's "SEE LICENSE IN
// <file>" is refused) and contain none of NOASSERTION, NONE, OTHER, UNKNOWN
// and UNLICENSED, compared case-insensitively.
func CheckAssertedLicense(expr string) error {
	if err := CheckLicense(expr); err != nil {
		return err
	}

	tokens := licenseTokens(expr)

	for _, token := range tokens {
		for _, value := range nonAssertions {
			if strings.EqualFold(token, value) {
				return fmt.Errorf("license %q asserts no license: %s grants no permission to redistribute", expr, token)
			}
		}
	}

	p := &licenseParser{tokens: tokens}
	if err := p.expression(); err != nil {
		return fmt.Errorf("license %q is not an SPDX license expression: %w", expr, err)
	}

	if p.pos < len(p.tokens) {
		return fmt.Errorf("license %q is not an SPDX license expression: unexpected %q", expr, p.tokens[p.pos])
	}

	return nil
}

// licenseTokens splits a license that passed CheckLicense into parentheses
// and words.
func licenseTokens(expr string) []string {
	var (
		tokens []string
		word   strings.Builder
	)

	flush := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}

	for _, r := range expr {
		switch r {
		case '(', ')':
			flush()

			tokens = append(tokens, string(r))
		case ' ':
			flush()
		default:
			word.WriteRune(r)
		}
	}

	flush()

	return tokens
}

// licenseParser checks the SPDX expression grammar:
//
//	expression = term { ("AND" | "OR") term }
//	term       = "(" expression ")" | identifier [ "WITH" identifier ]
type licenseParser struct {
	tokens []string
	pos    int
}

func (p *licenseParser) next() string {
	if p.pos >= len(p.tokens) {
		return ""
	}

	return p.tokens[p.pos]
}

func (p *licenseParser) expression() error {
	if err := p.term(); err != nil {
		return err
	}

	for p.next() == "AND" || p.next() == "OR" {
		p.pos++

		if err := p.term(); err != nil {
			return err
		}
	}

	return nil
}

func (p *licenseParser) term() error {
	if p.next() == "(" {
		p.pos++

		if err := p.expression(); err != nil {
			return err
		}

		if p.next() != ")" {
			return errors.New("missing )")
		}

		p.pos++

		return nil
	}

	if err := p.identifier(); err != nil {
		return err
	}

	if p.next() == "WITH" {
		p.pos++

		return p.identifier()
	}

	return nil
}

func (p *licenseParser) identifier() error {
	switch token := p.next(); token {
	case "":
		return errors.New("a license identifier is missing at the end")
	case "(", ")", "AND", "OR", "WITH":
		return fmt.Errorf("expected a license identifier, got %q", token)
	}

	p.pos++

	return nil
}

func checkText(field, value string, limit int, required bool) error {
	switch {
	case required && strings.TrimSpace(value) == "":
		return fmt.Errorf("%s is required", field)
	case len(value) > limit:
		return fmt.Errorf("%s is longer than %d bytes", field, limit)
	case !utf8.ValidString(value):
		return fmt.Errorf("%s is not valid UTF-8", field)
	}

	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return fmt.Errorf("%s contains control characters", field)
		}
	}

	return nil
}

type normalized struct {
	exact    string
	host     string
	path     string
	stem     string
	fragment bool
	portless bool
	query    bool
}

// normalize parses an http(s) URL for matching. Paths with dot segments or
// encoded separators are refused because servers may resolve them to a
// location outside a path_prefix the URL appears to be under. The exact key
// leaves out the scheme, like host rules do, so that an http spelling of a
// URL cannot bypass a rule written for https.
func normalize(raw string) (normalized, error) {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return normalized{}, err
	}

	escaped := u.EscapedPath()
	lower := strings.ToLower(escaped)

	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(u.Path, "\\") ||
		hasDotSegment(u.Path) {
		return normalized{}, errors.New("path must not contain dot segments or encoded separators")
	}

	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	p := foldGitHubRepository(host, cmp.Or(u.Path, "/"))
	canonical := (&url.URL{Path: p}).EscapedPath()
	hostPort := host

	if strings.Contains(host, ":") {
		hostPort = "[" + host + "]"
	}

	port := u.Port()
	portless := port == "" || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80")

	if !portless {
		hostPort += ":" + port
	}

	query := u.RawQuery != "" || u.ForceQuery

	exact := "//" + hostPort + canonical
	if query {
		exact += "?" + u.RawQuery
	}

	return normalized{
		exact:    exact,
		host:     host,
		path:     p,
		stem:     fileStem(p[strings.LastIndexByte(p, '/')+1:]),
		fragment: strings.Contains(raw, "#"),
		portless: portless,
		query:    query,
	}, nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > maxURLBytes {
		return nil, fmt.Errorf("must be a URL of 1-%d bytes", maxURLBytes)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("not a URL: %w", err)
	}

	switch {
	case u.Scheme != "https" && u.Scheme != "http":
		return nil, errors.New("scheme must be http or https")
	case u.User != nil:
		return nil, errors.New("must not contain credentials")
	case u.Opaque != "" || u.Hostname() == "":
		return nil, errors.New("must be an absolute URL with a host")
	}

	return u, nil
}

// githubHosts serve files of GitHub repositories under /<owner>/<repo>/.
var githubHosts = []string{"github.com", "raw.githubusercontent.com"}

// foldGitHubRepository lowercases the owner and repository segments of a
// path on a GitHub host. GitHub resolves those names in any letter case, so
// without folding a URL could name a file of a repository in a spelling a
// review rule's path_prefix does not match, and fall through to automatic
// license detection.
func foldGitHubRepository(host, p string) string {
	if !slices.Contains(githubHosts, host) {
		return p
	}

	segments := strings.SplitN(p, "/", 4)
	for i := 1; i < len(segments) && i <= 2; i++ {
		segments[i] = strings.ToLower(segments[i])
	}

	return strings.Join(segments, "/")
}

func hasDotSegment(p string) bool {
	return slices.ContainsFunc(strings.Split(p, "/"), func(segment string) bool {
		return segment == "." || segment == ".."
	})
}

func prefixMatches(p, prefix string) bool {
	switch {
	case prefix == "", p == prefix:
		return true
	case strings.HasSuffix(prefix, "/"):
		return strings.HasPrefix(p, prefix)
	default:
		return strings.HasPrefix(p, prefix+"/")
	}
}

type outcome struct {
	rule     *rule
	auto     *autoOutcome
	declared *declaration
	url      string
	problem  string
}

// declaration is a license a local source file declares for one URL; err
// is set when it asserts no license (see CheckAssertedLicense).
type declaration struct {
	license string
	err     error
}

func (o outcome) verdict() Verdict {
	switch {
	case o.declared != nil && o.declared.err != nil:
		return Review
	case o.declared != nil:
		return Allow
	case o.rule != nil:
		return o.rule.verdict
	case o.auto != nil:
		return o.auto.detection.Verdict
	default:
		return Review
	}
}

func (o outcome) reason(root bool) string {
	var text string

	switch {
	case o.declared != nil && o.declared.err != nil:
		text = "the local source file declares no usable license: " + o.declared.err.Error()
	case o.declared != nil:
		text = DeclaredReason
	case o.problem != "":
		text = o.problem
	case o.auto != nil:
		text = o.auto.reason
	case o.rule == nil:
		text = NoRuleReason
	default:
		text = o.rule.reason + " (rule " + o.rule.id + ")"
	}

	if root {
		return text
	}

	return "dependency " + o.url + ": " + text
}

func blocked(outcomes []outcome, verdict Verdict) (Decision, bool) {
	var (
		reasons    []string
		ruleID     string
		autoReason string
		found      bool
	)

	for i, o := range outcomes {
		if o.verdict() != verdict {
			continue
		}

		if !found {
			switch {
			case o.rule != nil:
				ruleID = o.rule.id
			case o.auto != nil:
				autoReason = o.auto.detection.Reason
			}
		}

		found = true

		reasons = append(reasons, o.reason(i == 0))
	}

	if !found {
		return Decision{}, false
	}

	return Decision{
		Decision: verdict, Reason: strings.Join(reasons, "; "), RuleID: ruleID, AutoReason: autoReason,
		Detections: detections(outcomes),
	}, true
}

// grant is what an allowing rule, detection or declaration contributes to a
// schema.
type grant struct {
	// key identifies the contribution: a rule ID, or for detections and
	// declarations a key no rule ID can take.
	key     string
	license string
	notice  string
	reason  string
}

func (o outcome) grant() grant {
	if o.declared != nil {
		return grant{key: "declared:" + o.declared.license, license: o.declared.license, reason: DeclaredReason}
	}

	if o.auto != nil {
		return grant{
			key: "auto:" + o.auto.detection.Source + "\x00" + o.auto.notice, license: o.auto.license, notice: o.auto.notice,
			reason: o.auto.reason,
		}
	}

	return grant{key: o.rule.id, license: o.rule.license, notice: o.rule.notice, reason: o.rule.reason + " (rule " + o.rule.id + ")"}
}

func allowed(outcomes []outcome) Decision {
	root := outcomes[0].grant()

	var others []grant

	for _, o := range outcomes[1:] {
		g := o.grant()
		if g.key != root.key && !slices.ContainsFunc(others, func(other grant) bool { return other.key == g.key }) {
			others = append(others, g)
		}
	}

	slices.SortFunc(others, func(a, b grant) int { return strings.Compare(a.key, b.key) })

	grants := make([]grant, 0, 1+len(others))
	grants = append(grants, root)
	grants = append(grants, others...)

	licenses := make([]string, 0, len(grants))
	notices := make([]string, 0, len(grants))
	reasons := make([]string, 0, len(grants))

	for _, g := range grants {
		if !slices.Contains(licenses, g.license) {
			licenses = append(licenses, g.license)
		}

		if g.notice != "" && !slices.Contains(notices, g.notice) {
			notices = append(notices, g.notice)
		}

		if !slices.Contains(reasons, g.reason) {
			reasons = append(reasons, g.reason)
		}
	}

	d := Decision{
		Decision:   Allow,
		License:    combineLicenses(licenses),
		Notice:     strings.Join(notices, "\n\n"),
		Reason:     strings.Join(reasons, "; "),
		Detections: detections(outcomes),
	}

	if outcomes[0].rule != nil {
		d.RuleID = outcomes[0].rule.id
	}

	if len(d.Notice) > MaxCombinedNoticeBytes {
		return Decision{
			Decision: Review, Detections: d.Detections,
			Reason: fmt.Sprintf("the notices of the schema's sources need %d bytes, more than the limit of %d", len(d.Notice), MaxCombinedNoticeBytes),
		}
	}

	return d
}

func detections(outcomes []outcome) []Detection {
	var out []Detection

	for _, o := range outcomes {
		if o.auto != nil {
			out = append(out, o.auto.detection)
		}
	}

	return out
}

func combineLicenses(licenses []string) string {
	slices.Sort(licenses)

	if len(licenses) == 1 {
		return licenses[0]
	}

	parts := make([]string, len(licenses))
	for i, license := range licenses {
		if strings.Contains(license, " OR ") {
			license = "(" + license + ")"
		}

		parts[i] = license
	}

	return strings.Join(parts, " AND ")
}

var metaschemaPaths = []string{
	"/draft-04/schema",
	"/draft-06/schema",
	"/draft-07/schema",
	"/draft/2019-09/schema",
	"/draft/2020-12/schema",
}

// IsWellKnownMetaschema reports whether uri is one of the official JSON
// Schema metaschemas (draft-04, draft-06, draft-07, 2019-09, 2020-12) on
// json-schema.org, over http or https, optionally followed by an empty
// fragment. Such URIs identify dialects that validators embed; they are not
// dependencies to fetch or license. A fragment pointing into a metaschema is
// not recognized.
func IsWellKnownMetaschema(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}

	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.EqualFold(u.Host, "json-schema.org") {
		return false
	}

	return slices.Contains(metaschemaPaths, u.EscapedPath())
}

// schemaStoreAlias reports a spelling of a SchemaStore-hosted schema other
// than the canonical one. Decisions only ever see the canonical URL, so an
// exact rule written with an alias would silently match nothing; for an
// exclude rule that means a takedown that never takes effect.
func schemaStoreAlias(key normalized) bool {
	switch key.host {
	case "json.schemastore.org", "schemastore.org":
		return true
	case "www.schemastore.org":
		return strings.HasPrefix(key.path, "/schemas/json/") || !strings.HasSuffix(key.path, ".json")
	case "raw.githubusercontent.com", "github.com":
		return strings.HasPrefix(key.path, "/schemastore/schemastore/")
	}

	return false
}
