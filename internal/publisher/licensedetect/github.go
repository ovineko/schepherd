package licensedetect

import (
	"context"
	// bearer:disable go_gosec_blocklist_sha1
	// Git names blobs by SHA-1; the digest checks GitHub's own consistency.
	"crypto/sha1" //nolint:gosec // Git names blobs by SHA-1; the digest checks GitHub's own consistency.
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/publisher/httpfetch"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

const (
	githubJSON = "application/vnd.github+json"
	githubSHA  = "application/vnd.github.sha"
)

var (
	ownerPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	refPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,254}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	noticeNames   = []string{"NOTICE", "NOTICE.txt", "NOTICE.md"}
)

type githubRepo struct {
	owner string
	repo  string
	// ref is the branch, tag or commit the URL names, without a refs/heads/
	// or refs/tags/ prefix: GitHub's raw redirects add the prefix, and both
	// spellings must share one finding and one notice text. refPath is the
	// form the commits endpoint resolves (heads/<name> or tags/<name> when
	// the URL qualifies the ref).
	ref     string
	refPath string
}

// githubSource parses <owner>/<repo>/<ref>/<path> (raw.githubusercontent.com)
// or <owner>/<repo>/raw|blob/<ref>/<path> (github.com). A ref is one path
// segment or refs/heads/<name> or refs/tags/<name>: a branch whose name
// contains "/" cannot be told apart from a directory and is read as the
// ref's first segment, which then fails to resolve or resolves to a
// different branch; either way the license comes from the repository the URL
// names.
func githubSource(segments []string, web bool) (source, string) {
	const unsupported = "the URL does not name a file of a GitHub repository at a ref"

	if len(segments) < 2 {
		return nil, unsupported
	}

	g := &githubRepo{owner: segments[0], repo: segments[1]}
	rest := segments[2:]

	if web {
		if len(rest) == 0 || (rest[0] != "raw" && rest[0] != "blob") {
			return nil, unsupported
		}

		rest = rest[1:]
	}

	if len(rest) >= 3 && rest[0] == "refs" && (rest[1] == "heads" || rest[1] == "tags") {
		g.ref, g.refPath, rest = rest[2], rest[1]+"/"+rest[2], rest[3:]
	} else if len(rest) > 0 {
		g.ref, g.refPath, rest = rest[0], rest[0], rest[1:]
	}

	switch {
	case !ownerPattern.MatchString(g.owner) || !repoPattern.MatchString(g.repo) || g.repo == "." || g.repo == "..":
		return nil, "the URL does not name a valid GitHub owner and repository"
	case len(rest) == 0 || rest[len(rest)-1] == "":
		return nil, unsupported
	case !refPattern.MatchString(g.ref) || strings.Contains(g.ref, ".."):
		return nil, "the GitHub ref " + strconv.Quote(g.ref) + " cannot be pinned"
	}

	return g, ""
}

func (g *githubRepo) key() string {
	return "github:" + g.slug() + "@" + g.ref
}

// slug is the repository in lowercase: GitHub names are case-insensitive,
// and one spelling keeps findings deterministic.
func (g *githubRepo) slug() string {
	return strings.ToLower(g.owner + "/" + g.repo)
}

// api returns the path of a repository endpoint. It uses the lowercase
// names as well, so the requests do not depend on which spelling of the
// repository reached the detector first.
func (g *githubRepo) api(suffix string) string {
	return "/repos/" + url.PathEscape(strings.ToLower(g.owner)) + "/" + url.PathEscape(strings.ToLower(g.repo)) + suffix
}

func (g *githubRepo) detect(ctx context.Context, d *Detector) (policy.Finding, error) {
	pending := "github:" + g.slug() + "@" + g.ref

	commit, problem, err := g.commit(ctx, d)
	if err != nil {
		return failed(ctx, pending, "resolve the ref", err)
	}

	if problem != "" {
		return policy.Finding{Source: pending, Failure: policy.RefusedFetchFailed, Detail: problem}, nil
	}

	src := "github:" + g.slug() + "@" + commit
	at := url.Values{"ref": {commit}}

	res, err := d.get(ctx, request{service: policy.HostGitHubAPI, path: g.api("/license"), query: at, accept: githubJSON})
	if err != nil {
		return failed(ctx, src, "read the license", err)
	}

	if res.status == http.StatusNotFound {
		return policy.Finding{Source: src, Failure: policy.RefusedNoLicense, Detail: "GitHub finds no license file in " + src}, nil
	}

	var doc struct {
		githubBlob

		License *struct {
			SPDXID string `json:"spdx_id"`
		} `json:"license"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(res.body, &doc); err != nil {
		return failed(ctx, src, "read the license", fmt.Errorf("unexpected response: %w", err))
	}

	license := "NOASSERTION"
	if doc.License != nil && doc.License.SPDXID != "" {
		license = doc.License.SPDXID
	}

	text, err := doc.decode()
	if err == nil {
		err = errBadPath(doc.Path)
	}

	if err != nil {
		return failed(ctx, src, "read the license file "+strconv.Quote(doc.Path), err)
	}

	f := policy.Finding{Source: src, License: license, LicenseFile: doc.Path, LicenseDigest: digest.FromBytes(text)}
	subject := "the GitHub repository " + g.slug() + " at " + g.ref

	var notice *file

	if namesApache(license) {
		found, ok, err := g.notice(ctx, d, at)
		if err != nil {
			return failed(ctx, src, "read the NOTICE file", err)
		}

		if ok {
			notice = &found
		}
	}

	return finish(f, subject, &file{name: doc.Path, data: text}, notice), nil
}

// commit resolves the ref to a commit; a problem is an answer that makes the
// ref unusable.
func (g *githubRepo) commit(ctx context.Context, d *Detector) (commit, problem string, err error) {
	if commitPattern.MatchString(g.ref) {
		return g.ref, "", nil
	}

	res, err := d.get(ctx, request{service: policy.HostGitHubAPI, path: g.api("/commits/" + escapeRef(g.refPath)), accept: githubSHA})
	if status, ok := errors.AsType[*httpfetch.StatusError](err); ok && status.Code == http.StatusUnprocessableEntity {
		// GitHub answers 422 for a name that is no commit, tag or branch.
		res, err = response{status: http.StatusNotFound}, nil
	}

	if err != nil {
		return "", "", err
	}

	if res.status == http.StatusNotFound {
		return g.renamedBranch(ctx, d)
	}

	commit = strings.TrimSpace(string(res.body))
	if !commitPattern.MatchString(commit) {
		return "", "GitHub did not resolve the ref " + strconv.Quote(g.ref) + " of " + g.slug() + " to a commit", nil
	}

	return commit, "", nil
}

// renamedBranch resolves a branch that was renamed, which raw file URLs
// keep serving while the commits and license endpoints no longer know the
// old name; the branches endpoint redirects to the new branch.
func (g *githubRepo) renamedBranch(ctx context.Context, d *Detector) (commit, problem string, err error) {
	missing := "the ref " + strconv.Quote(g.ref) + " does not exist in the GitHub repository " + g.slug()

	if strings.HasPrefix(g.refPath, "tags/") {
		return "", missing, nil
	}

	name := strings.TrimPrefix(g.refPath, "heads/")

	res, err := d.get(ctx, request{service: policy.HostGitHubAPI, path: g.api("/branches/" + escapeRef(name)), accept: githubJSON})
	if err != nil {
		return "", "", err
	}

	if res.status == http.StatusNotFound {
		return "", missing, nil
	}

	var branch struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if json.Unmarshal(res.body, &branch) != nil || !commitPattern.MatchString(branch.Commit.SHA) {
		//nolint:nilerr // an unusable answer is a problem of this ref, held for review like a missing ref, not an error of the run
		return "", "GitHub did not resolve the branch " + strconv.Quote(g.ref) + " of " + g.slug() + " to a commit", nil
	}

	return branch.Commit.SHA, "", nil
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}

	return strings.Join(parts, "/")
}

// notice returns the repository's NOTICE file at the commit, if it has
// one. The root listing names it (one request), the blob endpoint serves it.
func (g *githubRepo) notice(ctx context.Context, d *Detector, at url.Values) (file, bool, error) {
	res, err := d.get(ctx, request{service: policy.HostGitHubAPI, path: g.api("/contents/"), query: at, accept: githubJSON})
	if err != nil || res.status == http.StatusNotFound {
		return file{}, false, err
	}

	var entries []struct {
		Type string `json:"type"`
		Name string `json:"name"`
		SHA  string `json:"sha"`
	}
	if err := json.Unmarshal(res.body, &entries); err != nil {
		return file{}, false, fmt.Errorf("unexpected repository listing: %w", err)
	}

	for _, want := range noticeNames {
		for _, e := range entries {
			if !strings.EqualFold(e.Name, want) || e.Type == "dir" {
				continue
			}

			// A NOTICE that is a symbolic link or a submodule cannot be
			// read as a blob; skipping it would publish without the NOTICE
			// Apache-2.0 requires.
			if e.Type != "file" {
				return file{}, false, fmt.Errorf("the NOTICE entry %q is a %s, not a file", e.Name, e.Type)
			}

			if !commitPattern.MatchString(e.SHA) || !cleanFilePath(e.Name) {
				return file{}, false, fmt.Errorf("unexpected listing entry %q", e.Name)
			}

			blob, err := d.get(ctx, request{service: policy.HostGitHubAPI, path: g.api("/git/blobs/" + e.SHA), accept: githubJSON})
			if err != nil {
				return file{}, false, err
			}

			if blob.status == http.StatusNotFound {
				return file{}, false, fmt.Errorf("the blob of %s is missing", e.Name)
			}

			var doc githubBlob
			if err := json.Unmarshal(blob.body, &doc); err != nil {
				return file{}, false, fmt.Errorf("unexpected blob response: %w", err)
			}

			if doc.SHA != e.SHA {
				return file{}, false, fmt.Errorf("GitHub served blob %s for %s, the listing names %s", doc.SHA, e.Name, e.SHA)
			}

			data, err := doc.decode()
			if err != nil {
				return file{}, false, fmt.Errorf("unusable NOTICE file %q: %w", e.Name, err)
			}

			return file{name: e.Name, data: data}, true, nil
		}
	}

	return file{}, false, nil
}

// githubBlob is the content part of the license and blob endpoints.
type githubBlob struct {
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	Size     int64  `json:"size"`
}

// decode returns the blob's bytes after checking them against the size and
// Git blob ID GitHub reports.
func (b *githubBlob) decode() ([]byte, error) {
	if b.Encoding != "base64" {
		return nil, fmt.Errorf("encoding %q is not base64", b.Encoding)
	}

	data, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(b.Content))
	if err != nil {
		return nil, fmt.Errorf("content is not base64: %w", err)
	}

	if int64(len(data)) != b.Size {
		return nil, fmt.Errorf("content has %d bytes, GitHub reports %d", len(data), b.Size)
	}

	if id := gitBlobID(data); id != b.SHA {
		return nil, fmt.Errorf("content has blob ID %s, GitHub reports %s", id, b.SHA)
	}

	return data, nil
}

func gitBlobID(data []byte) string {
	// bearer:disable go_gosec_crypto_weak_crypto
	// Git names blobs by SHA-1.
	h := sha1.New() //nolint:gosec // Git names blobs by SHA-1.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)

	// bearer:disable go_lang_weak_hash_sha1
	// The Git blob ID, not a security digest.
	return hex.EncodeToString(h.Sum(nil))
}

func cleanFilePath(p string) bool {
	return p != "" && len(p) <= 255 && path.Clean(p) == p && !path.IsAbs(p) && !strings.HasPrefix(p, "../") && p != ".." &&
		!strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' })
}

func errBadPath(p string) error {
	if cleanFilePath(p) {
		return nil
	}

	return fmt.Errorf("path %q is not a clean relative path", p)
}

// namesApache reports whether a license expression mentions Apache-2.0, the
// license whose section 4(d) makes NOTICE files travel with copies.
func namesApache(expr string) bool {
	for _, token := range strings.FieldsFunc(expr, func(r rune) bool { return r == ' ' || r == '(' || r == ')' }) {
		if strings.EqualFold(token, "Apache-2.0") {
			return true
		}
	}

	return false
}
