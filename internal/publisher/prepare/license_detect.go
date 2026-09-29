package prepare

import (
	"context"
	"time"

	"github.com/ovineko/schepherd/internal/env"
	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/licensedetect"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

// enableDetection gives the decider a license detector when the policy's
// [auto] section is enabled: det when it is not nil, otherwise one limited
// to the policy's hosts that sends the token from GITHUB_TOKEN to the GitHub
// API. Detection contacts fixed public services, so it does not use the
// source's fetch settings. jobs bounds its concurrent requests.
func (d *decider) enableDetection(det *licensedetect.Detector, jobs int, log func(string, ...any)) error {
	if d.policy == nil || !d.policy.Auto().Enabled {
		return nil
	}

	if det == nil {
		token, _, err := env.GitHubToken()
		if err != nil {
			return fault.Wrap(fault.Usage, err, "automatic license detection")
		}

		det, err = licensedetect.New(detectionConfig(d.policy.Auto(), token, jobs, log))
		if err != nil {
			return fault.Wrap(fault.Usage, err, "automatic license detection")
		}
	}

	d.detector = det

	return nil
}

// Rate limits detection waits out. GitHub's limit resets every hour; with a
// token (the weekly update passes the workflow's, whose 1,000 requests per
// hour a full SchemaStore import comes close to) that reset is waited for,
// because every record detection cannot decide stays out of the catalog (or,
// when published, is held unrefreshed) until the next run. Without one the
// anonymous limit of 60 requests per hour would stall a run for many hours,
// so only short Retry-After pauses are waited for.
const (
	authenticatedRateLimitWait = 65 * time.Minute
	anonymousRateLimitWait     = 2 * time.Minute
)

func detectionConfig(auto policy.Auto, token string, jobs int, log func(string, ...any)) licensedetect.Config {
	wait := anonymousRateLimitWait
	if token != "" {
		wait = authenticatedRateLimitWait
	}

	return licensedetect.Config{Hosts: auto.Hosts, Token: token, Jobs: jobs, Log: log, RateLimitWait: wait}
}

// detectLicenses runs automatic license detection for the URIs that neither
// a rule nor a local declaration decides, so that decide and single find
// their findings. Detection problems become findings that hold the URI for
// review; only cancellation is an error.
func (d *decider) detectLicenses(ctx context.Context, uris ...string) error {
	if d.detector == nil {
		return nil
	}

	for _, uri := range uris {
		canonical := d.canon(uri)

		if policy.IsWellKnownMetaschema(uri) || d.policy.HasRule(canonical) {
			continue
		}

		if _, declared := d.declared[normalizeURI(uri)]; declared {
			continue
		}

		if _, err := d.detector.Detect(ctx, canonical); err != nil {
			return err //nolint:wrapcheck // fault.Canceled from package licensedetect
		}
	}

	return nil
}

// decideContext is decide after detecting the licenses it needs.
func (d *decider) decideContext(ctx context.Context, root string, deps []string) (policy.Decision, error) {
	if err := d.detectLicenses(ctx, append([]string{root}, deps...)...); err != nil {
		return policy.Decision{}, err
	}

	return d.decide(root, deps), nil
}

// singleContext is single after detecting the license it needs.
func (d *decider) singleContext(ctx context.Context, uri string) (policy.Decision, error) {
	if err := d.detectLicenses(ctx, uri); err != nil {
		return policy.Decision{}, err
	}

	return d.single(uri), nil
}

// lookup gives the policy the findings detection already has. A URI whose
// source was not detected yet has none and stays unruled; this is what a
// redirect target gets, which is vetted before it is contacted, unless it
// belongs to the repository ref or package version of a detected source.
func (d *decider) lookup() policy.Lookup {
	if d.detector == nil {
		return nil
	}

	return d.detector.Cached
}
