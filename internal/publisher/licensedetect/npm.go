package licensedetect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/ovineko/schepherd/internal/digest"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

var (
	packagePattern = regexp.MustCompile(`^(?:@[a-z0-9~-][a-z0-9._~-]*/)?[A-Za-z0-9~-][A-Za-z0-9._~-]*$`)
	// licenseFiles are probed in this order on the CDN, which has no
	// listing on the allowed hosts; npm always packs such files.
	licenseFiles = []string{
		"LICENSE", "LICENSE.md", "LICENSE.txt", "license", "license.md", "license.txt",
		"LICENCE", "LICENCE.md", "LICENCE.txt", "License", "License.md", "License.txt",
	}
	npmNoticeFiles = []string{"NOTICE", "NOTICE.txt", "NOTICE.md", "notice", "notice.txt", "notice.md"}
)

const maxPackageName = 214

type npmPackage struct {
	cdn     string
	name    string
	version string
}

// npmSource parses <package>@<version>/<path> where <package> may be
// scoped (@scope/name).
func npmSource(cdn string, segments []string) (source, string) {
	const unsupported = "the URL does not name a file of an npm package at a version"

	nameSegments := 1
	if len(segments) > 0 && strings.HasPrefix(segments[0], "@") {
		nameSegments = 2
	}

	if len(segments) <= nameSegments || segments[len(segments)-1] == "" {
		return nil, unsupported
	}

	spec := strings.Join(segments[:nameSegments], "/")

	at := strings.LastIndexByte(spec, '@')
	if at <= 0 {
		return nil, "the npm package URL names no exact version"
	}

	p := &npmPackage{cdn: cdn, name: spec[:at], version: spec[at+1:]}

	switch {
	case len(p.name) > maxPackageName || !packagePattern.MatchString(p.name):
		return nil, "the URL does not name a valid npm package"
	case !exactVersion(p.version):
		return nil, "the npm package URL names no exact version (" + p.version + ")"
	}

	return p, ""
}

// exactVersion accepts exact SemVer versions as the npm registry stores them:
// never a range, tag or shorthand, and never build metadata, which npm strips
// on publish (Canonical drops it, so such a version does not round-trip).
func exactVersion(version string) bool {
	return semver.Canonical("v"+version) == "v"+version
}

func (p *npmPackage) key() string {
	return "npm:" + p.name + "@" + p.version + " via " + p.cdn
}

func (p *npmPackage) cdnPath(name string) string {
	prefix := ""
	if p.cdn == policy.HostJSDelivr {
		prefix = "/npm"
	}

	return prefix + "/" + p.name + "@" + p.version + "/" + name
}

func (p *npmPackage) detect(ctx context.Context, d *Detector) (policy.Finding, error) {
	src := "npm:" + p.name + "@" + p.version

	license, problem, err := p.declared(ctx, d)
	if err != nil {
		return failed(ctx, src, "read the npm registry metadata", err)
	}

	if problem != nil {
		problem.Source = src

		return *problem, nil
	}

	f := policy.Finding{Source: src, License: license}

	text, found, err := p.firstFile(ctx, d, licenseFiles)
	if err != nil {
		return failed(ctx, src, "read the license file from "+p.cdn, err)
	}

	if !found {
		f.Failure, f.Detail = policy.RefusedNoLicense, "the package "+src+" has no license file on "+p.cdn

		return f, nil
	}

	f.LicenseFile, f.LicenseDigest = text.name, digest.FromBytes(text.data)

	var notice *file

	if namesApache(license) {
		found, ok, err := p.firstFile(ctx, d, npmNoticeFiles)
		if err != nil {
			return failed(ctx, src, "read the NOTICE file from "+p.cdn, err)
		}

		if ok {
			notice = &found
		}
	}

	return finish(f, "the npm package "+p.name+" version "+p.version, &text, notice), nil
}

// declared reads the license field of the version's registry metadata.
func (p *npmPackage) declared(ctx context.Context, d *Detector) (string, *policy.Finding, error) {
	escaped := strings.Replace(p.name, "/", "%2f", 1)

	res, err := d.get(ctx, request{service: policy.HostNPMRegistry, path: "/" + escaped + "/" + p.version, accept: "application/json"})
	if err != nil {
		return "", nil, err
	}

	if res.status == http.StatusNotFound {
		return "", &policy.Finding{
			Failure: policy.RefusedFetchFailed, Detail: "the npm registry has no " + p.name + "@" + p.version,
		}, nil
	}

	var meta struct {
		Name     string            `json:"name"`
		Version  string            `json:"version"`
		License  json.RawMessage   `json:"license"`
		Licenses []json.RawMessage `json:"licenses"`
	}
	if err := json.Unmarshal(res.body, &meta); err != nil {
		return "", nil, fmt.Errorf("unexpected response: %w", err)
	}

	if meta.Name != p.name || meta.Version != p.version {
		return "", nil, fmt.Errorf("the registry answered with %s@%s", meta.Name, meta.Version)
	}

	if license, ok := licenseValue(meta.License); ok {
		return license, nil, nil
	}

	switch len(meta.Licenses) {
	case 0:
		return "", &policy.Finding{Failure: policy.RefusedNoLicense, Detail: "the npm metadata of " + p.name + "@" + p.version + " declares no license"}, nil
	case 1:
		if license, ok := licenseValue(meta.Licenses[0]); ok {
			return license, nil, nil
		}
	}

	return "", &policy.Finding{
		Failure: policy.RefusedNotAsserted,
		Detail:  "the npm metadata of " + p.name + "@" + p.version + " lists licenses in the deprecated licenses array, which does not say how they combine",
	}, nil
}

// licenseValue reads a license field: an SPDX expression string, or the
// deprecated {"type": ...} object.
func licenseValue(raw json.RawMessage) (string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", false
	}

	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}

	var object struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Type != "" {
		return object.Type, true
	}

	return "", false
}

// firstFile returns the first of names the CDN serves for the version.
func (p *npmPackage) firstFile(ctx context.Context, d *Detector, names []string) (file, bool, error) {
	for _, name := range names {
		res, err := d.get(ctx, request{service: p.cdn, path: p.cdnPath(name), accept: "*/*"})
		if err != nil {
			return file{}, false, err
		}

		if res.status == http.StatusOK {
			return file{name: name, data: res.body}, true, nil
		}
	}

	return file{}, false, nil
}
