package licensedetect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/policy"
)

// pausingDetector is a detector with a fake clock whose sleeps advance the
// clock and call onSleep, which plays the rate-limit reset.
type pausingDetector struct {
	*Detector

	clock   time.Time
	onSleep func()
	logs    []string
	sleeps  []time.Duration
	mu      sync.Mutex
}

func newPausingDetector(t *testing.T, cfg Config, onSleep func()) *pausingDetector {
	t.Helper()

	p := &pausingDetector{clock: time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC), onSleep: onSleep}

	cfg.Log = func(format string, args ...any) {
		p.mu.Lock()
		defer p.mu.Unlock()

		p.logs = append(p.logs, fmt.Sprintf(format, args...))
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	d.now = func() time.Time {
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.clock
	}
	d.sleep = func(ctx context.Context, wait time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		p.mu.Lock()
		p.sleeps = append(p.sleeps, wait)
		p.clock = p.clock.Add(wait)
		p.mu.Unlock()

		if p.onSleep != nil {
			p.onSleep()
		}

		return nil
	}

	p.Detector = d

	return p
}

func (p *pausingDetector) current() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.clock
}

func githubLimit(reset time.Time) reply {
	return reply{
		status: http.StatusForbidden, body: `{"message":"API rate limit exceeded"}`,
		header: map[string]string{"X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": strconv.FormatInt(reset.Unix(), 10)},
	}
}

func TestRateLimitIsWaitedOut(t *testing.T) {
	f := newFakes(t)
	f.repo(t, "MIT", mitText)

	cfg := f.config()
	cfg.Token = "ghs_token"
	cfg.RateLimitWait = 65 * time.Minute

	p := newPausingDetector(t, cfg, func() { f.github.set("/repos/owner/repo/commits/main", reply{body: commit}) })
	f.github.set("/repos/owner/repo/commits/main", githubLimit(p.current().Add(30*time.Minute)))

	got := detect(t, p.Detector, rawURL)
	if got.Failure != "" || got.License != "MIT" || got.Source != "github:owner/repo@"+commit {
		t.Fatalf("finding after the reset = %#v", got)
	}

	if len(p.sleeps) != 1 || p.sleeps[0] != 30*time.Minute+resetMargin {
		t.Errorf("sleeps = %v, want one until the reset", p.sleeps)
	}

	if n := f.github.hitCount("/repos/owner/repo/commits/main"); n != 2 {
		t.Errorf("commits/main requested %d times, want 2", n)
	}

	if len(p.logs) != 1 || !strings.Contains(p.logs[0], "waits until 2026-09-24T03:30:01Z") {
		t.Errorf("logs = %v", p.logs)
	}
}

func TestRateLimitWaitIsBounded(t *testing.T) {
	t.Run("reset later than the wait", func(t *testing.T) {
		f := newFakes(t)
		f.repo(t, "MIT", mitText)

		cfg := f.config()
		cfg.RateLimitWait = 10 * time.Minute

		p := newPausingDetector(t, cfg, nil)
		f.github.set("/repos/owner/repo/commits/main", githubLimit(p.current().Add(30*time.Minute)))

		got := detect(t, p.Detector, rawURL)
		if got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, "rate limit exceeded") {
			t.Fatalf("finding = %#v", got)
		}

		if len(p.sleeps) != 0 || f.github.total() != 1 {
			t.Errorf("sleeps = %v, GitHub requests = %d; a later reset must stop the service at once", p.sleeps, f.github.total())
		}

		if other := detect(t, p.Detector, "https://raw.githubusercontent.com/other/lib/v1/x.json"); other.Failure != policy.RefusedFetchFailed ||
			f.github.total() != 1 {
			t.Errorf("after the stop: finding %#v, GitHub requests %d", other, f.github.total())
		}
	})

	t.Run("a service that keeps refusing", func(t *testing.T) {
		f := newFakes(t)

		cfg := f.config()
		cfg.RateLimitWait = time.Hour

		var p *pausingDetector

		p = newPausingDetector(t, cfg, func() {
			f.github.set("/repos/owner/repo/commits/main", githubLimit(p.current().Add(20*time.Minute)))
		})
		f.github.set("/repos/owner/repo/commits/main", githubLimit(p.current().Add(20*time.Minute)))

		got := detect(t, p.Detector, rawURL)
		if got.Failure != policy.RefusedFetchFailed || !strings.Contains(got.Detail, "rate limit exceeded") {
			t.Fatalf("finding = %#v", got)
		}

		if len(p.sleeps) != maxPauses || f.github.total() != maxPauses+1 {
			t.Errorf("sleeps = %v, GitHub requests = %d; want %d pauses and %d requests", p.sleeps, f.github.total(), maxPauses, maxPauses+1)
		}

		if last := p.logs[len(p.logs)-1]; !strings.Contains(last, "no further requests") {
			t.Errorf("logs = %v", p.logs)
		}
	})

	t.Run("no wait configured", func(t *testing.T) {
		f := newFakes(t)

		p := newPausingDetector(t, f.config(), nil)
		f.github.set("/repos/owner/repo/commits/main", githubLimit(p.current().Add(time.Second)))

		if got := detect(t, p.Detector, rawURL); got.Failure != policy.RefusedFetchFailed || len(p.sleeps) != 0 {
			t.Fatalf("finding = %#v, sleeps = %v", got, p.sleeps)
		}
	})
}

func TestRetryAfterIsWaitedOut(t *testing.T) {
	for name, value := range map[string]func(now time.Time) string{
		"seconds":   func(time.Time) string { return "90" },
		"HTTP date": func(now time.Time) string { return now.Add(90 * time.Second).Format(http.TimeFormat) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakes(t)
			meta := jsonReply(t, map[string]any{"name": "pkg", "version": "1.0.0", "license": "MIT"})
			f.unpkg.set("/pkg@1.0.0/LICENSE", reply{body: mitText})

			cfg := f.config()
			cfg.RateLimitWait = 2 * time.Minute

			p := newPausingDetector(t, cfg, func() { f.registry.set("/pkg/1.0.0", meta) })
			f.registry.set("/pkg/1.0.0", reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": value(p.current())}})

			got := detect(t, p.Detector, "https://unpkg.com/pkg@1.0.0/x.json")
			if got.Failure != "" || got.License != "MIT" {
				t.Fatalf("finding = %#v", got)
			}

			if len(p.sleeps) != 1 || p.sleeps[0] != 90*time.Second+resetMargin {
				t.Errorf("sleeps = %v", p.sleeps)
			}
		})
	}
}

func TestConcurrentRefusalsShareOnePause(t *testing.T) {
	f := newFakes(t)

	cfg := f.config()
	cfg.RateLimitWait = time.Hour
	cfg.Jobs = 4

	urls := make([]string, 0, 4)

	for i := range 4 {
		repo := fmt.Sprintf("repo%d", i)
		urls = append(urls, "https://raw.githubusercontent.com/owner/"+repo+"/main/x.json")
		f.github.set("/repos/owner/"+repo+"/license?ref="+commit, licenseReply(t, "LICENSE", "MIT", mitText))
	}

	p := newPausingDetector(t, cfg, nil)

	for i := range 4 {
		f.github.set(fmt.Sprintf("/repos/owner/repo%d/commits/main", i), githubLimit(p.current().Add(10*time.Minute)))
	}

	// The four refusals are noted before any sleeper returns: the clock
	// moves, and the limit resets, only once all four sleep.
	var (
		barrier  sync.Mutex
		arrived  int
		released = make(chan struct{})
	)

	p.sleep = func(_ context.Context, wait time.Duration) error {
		barrier.Lock()
		arrived++

		if arrived == len(urls) {
			for i := range 4 {
				f.github.set(fmt.Sprintf("/repos/owner/repo%d/commits/main", i), reply{body: commit})
			}

			p.mu.Lock()
			p.clock = p.clock.Add(wait)
			p.mu.Unlock()
			close(released)
		}
		barrier.Unlock()

		select {
		case <-released:
			return nil
		case <-time.After(10 * time.Second):
			return errors.New("not every request was refused")
		}
	}

	var wg sync.WaitGroup

	results := make([]policy.Finding, len(urls))

	for i, u := range urls {
		wg.Go(func() {
			results[i], _ = p.Detect(t.Context(), u)
		})
	}

	wg.Wait()

	for i, got := range results {
		if got.Failure != "" || got.License != "MIT" {
			t.Errorf("%s: finding = %#v", urls[i], got)
		}
	}

	if p.pauses[policy.HostGitHubAPI] != 1 {
		t.Errorf("pauses = %d; refusals of one limit must share one pause", p.pauses[policy.HostGitHubAPI])
	}
}

func TestCanceledWhilePaused(t *testing.T) {
	f := newFakes(t)

	cfg := f.config()
	cfg.RateLimitWait = time.Hour

	ctx, cancel := context.WithCancel(t.Context())

	p := newPausingDetector(t, cfg, nil)
	p.sleep = func(context.Context, time.Duration) error {
		cancel()

		return context.Canceled
	}

	f.github.set("/repos/owner/repo/commits/main", githubLimit(p.current().Add(time.Minute)))

	_, err := p.Detect(ctx, rawURL)
	if fault.KindOf(err) != fault.Canceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("Detect = %v, want fault.Canceled", err)
	}

	if _, ok := p.Cached(rawURL); ok {
		t.Error("a canceled detection was remembered")
	}
}

func TestSleep(t *testing.T) {
	if err := sleep(t.Context(), time.Millisecond); err != nil {
		t.Fatalf("sleep = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep after cancel = %v", err)
	}
}
