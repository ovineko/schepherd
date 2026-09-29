package regproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const blobPath = "/v2/org/schemas/blobs/sha256:4d2f0f4c63d09c3a9a0bd4bc0ca11b6a4d88f0bcb4bb8f3a0ab5c5d15e1e0b11"

type upstream struct {
	*httptest.Server

	hits atomic.Int64
}

func newUpstream(t *testing.T, handler http.HandlerFunc) *upstream {
	t.Helper()

	up := &upstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(up.Close)

	return up
}

func startProxy(t *testing.T, target string, opts ...Option) (*Proxy, string) {
	t.Helper()

	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}

	p := New(u, opts...)

	base, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(p.Close)

	return p, base
}

func settled(t *testing.T, p *Proxy) []Record {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if err := p.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}

	return p.Records()
}

func settledStats(t *testing.T, p *Proxy) Stats {
	t.Helper()
	settled(t, p)

	return p.Stats()
}

func newClient(t *testing.T) *http.Client {
	t.Helper()

	transport := &http.Transport{DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport}
}

type call struct {
	method string
	target string
	body   io.Reader
	header http.Header
	host   string
}

// reply keeps what the tests inspect after the response body is closed.
type reply struct {
	StatusCode    int
	ContentLength int64
	Header        http.Header
}

func send(t *testing.T, client *http.Client, c call) (reply, []byte, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), c.method, c.target, c.body)
	if err != nil {
		t.Fatal(err)
	}

	maps.Copy(req.Header, c.header)

	if c.host != "" {
		req.Host = c.host
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", c.method, c.target, err)
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)

	return reply{StatusCode: resp.StatusCode, ContentLength: resp.ContentLength, Header: resp.Header}, data, err
}

func mustSend(t *testing.T, client *http.Client, c call) (reply, []byte) {
	t.Helper()

	resp, data, err := send(t, client, c)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", c.method, c.target, err)
	}

	return resp, data
}

func pattern(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7 + i/251)
	}

	return out
}

func serveBlob(content []byte) http.HandlerFunc {
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	}
}

func serveChunked(content []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		_, _ = w.Write(content)
	}
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)

			return
		}

		next(w, r)
	}
}

func authHeader() http.Header {
	return http.Header{"Authorization": {"Basic dXNlcjpwYXNz"}}
}

func TestRecordsEveryRoute(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	p, base := startProxy(t, up.URL)
	client := newClient(t)

	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	calls := []struct {
		method string
		path   string
		query  string
		class  string
	}{
		{http.MethodGet, "/v2/", "", ClassPing},
		{http.MethodHead, "/v2/", "", ClassPing},
		{http.MethodGet, "/v2/org/schemas/manifests/" + digest, "", ClassManifestGet},
		{http.MethodHead, "/v2/org/schemas/manifests/catalog-latest", "", ClassManifestHead},
		{http.MethodPut, "/v2/org/schemas/manifests/catalog-20260923.1", "", ClassManifestPut},
		{http.MethodDelete, "/v2/org/schemas/manifests/" + digest, "", ClassManifestDelete},
		{http.MethodGet, "/v2/org/schemas/blobs/" + digest, "", ClassBlobGet},
		{http.MethodHead, "/v2/org/schemas/blobs/" + digest, "", ClassBlobHead},
		{http.MethodPost, "/v2/org/schemas/blobs/uploads/", "mount=" + digest + "&from=other", ClassBlobUpload},
		{http.MethodPatch, "/v2/org/schemas/blobs/uploads/uuid-1", "_state=abc", ClassBlobUpload},
		{http.MethodPut, "/v2/org/schemas/blobs/uploads/uuid-1", "digest=" + digest, ClassBlobUpload},
		{http.MethodGet, "/v2/org/schemas/tags/list", "n=100", ClassTags},
		{http.MethodGet, "/v2/org/schemas/referrers/" + digest, "artifactType=x", ClassReferrers},
		{http.MethodGet, "/v2/_catalog", "", ClassCatalog},
		{http.MethodGet, "/debug/health", "", ClassOther},
	}

	for _, c := range calls {
		target := base + c.path
		if c.query != "" {
			target += "?" + c.query
		}

		if resp, _ := mustSend(t, client, call{method: c.method, target: target}); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status %d", c.method, c.path, resp.StatusCode)
		}
	}

	records := settled(t, p)
	if len(records) != len(calls) {
		t.Fatalf("got %d records, want %d", len(records), len(calls))
	}

	byClass := map[string]int{}

	for i, c := range calls {
		rec := records[i]
		byClass[c.class]++

		if rec.Method != c.method || rec.Path != c.path || rec.Query != c.query || rec.Class != c.class {
			t.Errorf("record %d = %+v, want %s %s?%s as %s", i, rec, c.method, c.path, c.query, c.class)
		}

		if rec.Status != http.StatusOK || rec.Fault != "" || rec.HasAuthorization || rec.Time.IsZero() {
			t.Errorf("record %d = %+v", i, rec)
		}

		wantResp := int64(2)
		if c.method == http.MethodHead {
			wantResp = 0
		}

		if rec.RespBytes != wantResp {
			t.Errorf("record %d RespBytes %d, want %d", i, rec.RespBytes, wantResp)
		}
	}

	stats := settledStats(t, p)
	if stats.Total != len(calls) || len(stats.ByClass) != len(byClass) {
		t.Fatalf("stats %+v", stats)
	}

	for class, n := range byClass {
		if stats.ByClass[class] != n {
			t.Errorf("ByClass[%s] = %d, want %d", class, stats.ByClass[class], n)
		}
	}

	if up.hits.Load() != int64(len(calls)) {
		t.Fatalf("upstream saw %d requests, want %d", up.hits.Load(), len(calls))
	}
}

func TestCounters(t *testing.T) {
	t.Parallel()

	content := pattern(1000)
	up := newUpstream(t, requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			n, _ := io.Copy(io.Discard, r.Body)
			w.Header().Set("Range", "0-"+strconv.FormatInt(n-1, 10))
			w.WriteHeader(http.StatusAccepted)

			return
		}

		serveBlob(content)(w, r)
	}))
	p, base := startProxy(t, up.URL)
	client := newClient(t)

	if resp, _ := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}

	if _, body := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath, header: authHeader()}); !bytes.Equal(body, content) {
		t.Fatal("blob content changed in transit")
	}

	upload := base + "/v2/org/schemas/blobs/uploads/uuid-1"
	mustSend(t, client, call{method: http.MethodPatch, target: upload, body: bytes.NewReader(pattern(4096)), header: authHeader()})
	mustSend(t, client, call{method: http.MethodPatch, target: upload, body: io.MultiReader(bytes.NewReader(pattern(3000))), header: authHeader()})
	mustSend(t, client, call{method: http.MethodHead, target: base + blobPath, header: authHeader()})

	records := settled(t, p)
	if len(records) != 5 {
		t.Fatalf("got %d records", len(records))
	}

	if records[0].Status != http.StatusUnauthorized || records[0].HasAuthorization || records[0].RespBytes == 0 {
		t.Errorf("challenge record %+v", records[0])
	}

	if records[1].Status != http.StatusOK || !records[1].HasAuthorization || records[1].RespBytes != int64(len(content)) {
		t.Errorf("blob record %+v", records[1])
	}

	if records[2].ReqBytes != 4096 || records[2].Status != http.StatusAccepted || records[2].Class != ClassBlobUpload {
		t.Errorf("sized upload record %+v", records[2])
	}

	if records[3].ReqBytes != 3000 {
		t.Errorf("chunked upload record %+v", records[3])
	}

	if records[4].Class != ClassBlobHead || records[4].RespBytes != 0 {
		t.Errorf("head record %+v", records[4])
	}

	want := Stats{
		ByClass:           map[string]int{ClassBlobGet: 2, ClassBlobUpload: 2, ClassBlobHead: 1},
		Total:             5,
		AuthChallenges:    1,
		WithAuthorization: 4,
		BlobBytesOut:      int64(len(content)),
	}
	assertStats(t, settledStats(t, p), want)

	p.Reset()

	if len(settled(t, p)) != 0 {
		t.Fatal("Reset kept records")
	}

	assertStats(t, settledStats(t, p), Stats{ByClass: map[string]int{}})
}

func assertStats(t *testing.T, got, want Stats) {
	t.Helper()

	if got.Total != want.Total || got.AuthChallenges != want.AuthChallenges ||
		got.WithAuthorization != want.WithAuthorization || got.BlobBytesOut != want.BlobBytesOut ||
		len(got.ByClass) != len(want.ByClass) {
		t.Fatalf("stats %+v, want %+v", got, want)
	}

	for class, n := range want.ByClass {
		if got.ByClass[class] != n {
			t.Fatalf("stats %+v, want %+v", got, want)
		}
	}
}

func TestRecordsReturnsCopy(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	p, base := startProxy(t, up.URL)
	mustSend(t, newClient(t), call{method: http.MethodGet, target: base + "/v2/"})

	records := settled(t, p)
	records[0].Class = "mutated"

	if settled(t, p)[0].Class != ClassPing {
		t.Fatal("Records exposes internal state")
	}
}

func TestFaultStatus(t *testing.T) {
	t.Parallel()

	manifest := "/v2/org/schemas/manifests/latest"

	tests := []struct {
		name   string
		fault  Fault
		calls  []string
		status []int
		hits   int64
	}{
		{
			name:   "limited",
			fault:  Fault{Name: "flaky", Class: ClassManifestGet, Status: http.StatusServiceUnavailable, Times: 2},
			calls:  []string{"GET " + manifest, "GET " + manifest, "GET " + manifest},
			status: []int{503, 503, 200},
			hits:   1,
		},
		{
			name:   "unlimited",
			fault:  Fault{Name: "broken", Class: ClassManifestGet, Status: http.StatusTooManyRequests},
			calls:  []string{"GET " + manifest, "GET " + manifest, "GET " + manifest, "GET " + manifest},
			status: []int{429, 429, 429, 429},
			hits:   0,
		},
		{
			name:   "default status",
			fault:  Fault{Name: "default"},
			calls:  []string{"GET /v2/"},
			status: []int{500},
			hits:   0,
		},
		{
			name:   "method is case-insensitive",
			fault:  Fault{Name: "head-only", Method: "head", Status: http.StatusNotFound},
			calls:  []string{"GET " + manifest, "HEAD " + manifest},
			status: []int{200, 404},
			hits:   1,
		},
		{
			name:   "path substring",
			fault:  Fault{Name: "other-repo", PathContains: "/other/", Status: http.StatusForbidden},
			calls:  []string{"GET " + manifest, "GET /v2/other/manifests/latest"},
			status: []int{200, 403},
			hits:   1,
		},
		{
			name:   "class mismatch",
			fault:  Fault{Name: "blobs", Class: ClassBlobGet, Status: http.StatusBadGateway},
			calls:  []string{"GET " + manifest, "GET /v2/"},
			status: []int{200, 200},
			hits:   2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "{}")
			})
			p, base := startProxy(t, up.URL)
			p.AddFault(tc.fault)
			client := newClient(t)

			for i, line := range tc.calls {
				method, path, _ := strings.Cut(line, " ")
				resp, body := mustSend(t, client, call{method: method, target: base + path})

				if resp.StatusCode != tc.status[i] {
					t.Fatalf("call %d: status %d, want %d", i, resp.StatusCode, tc.status[i])
				}

				rec := settled(t, p)[i]
				injected := resp.StatusCode != http.StatusOK

				if (rec.Fault == tc.fault.Name) != injected || rec.Status != resp.StatusCode {
					t.Fatalf("call %d: record %+v", i, rec)
				}

				if injected && method == http.MethodGet {
					var parsed errorBody
					if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Errors) != 1 ||
						!strings.Contains(parsed.Errors[0].Message, tc.fault.Name) {
						t.Fatalf("call %d: error body %q", i, body)
					}
				}
			}

			if got := up.hits.Load(); got != tc.hits {
				t.Fatalf("upstream saw %d requests, want %d", got, tc.hits)
			}
		})
	}
}

func TestClearFaultsAndOrder(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	p, base := startProxy(t, up.URL)
	client := newClient(t)

	p.AddFault(Fault{Name: "first", Class: ClassPing, Status: http.StatusTeapot, Times: 1})
	p.AddFault(Fault{Name: "second", Class: ClassPing, Status: http.StatusConflict})

	for _, want := range []int{http.StatusTeapot, http.StatusConflict, http.StatusConflict} {
		if resp, _ := mustSend(t, client, call{method: http.MethodGet, target: base + "/v2/"}); resp.StatusCode != want {
			t.Fatalf("status %d, want %d", resp.StatusCode, want)
		}
	}

	p.ClearFaults()

	if resp, _ := mustSend(t, client, call{method: http.MethodGet, target: base + "/v2/"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d after ClearFaults", resp.StatusCode)
	}

	p.Reset()
	p.AddFault(Fault{Name: "kept", Status: http.StatusGone})

	if resp, _ := mustSend(t, client, call{method: http.MethodGet, target: base + "/v2/"}); resp.StatusCode != http.StatusGone {
		t.Fatalf("Reset dropped faults: status %d", resp.StatusCode)
	}
}

func diffOffsets(a, b []byte) []int {
	var out []int

	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			out = append(out, i)
		}
	}

	return out
}

func TestFaultCorruptBody(t *testing.T) {
	t.Parallel()

	content := pattern(1001)
	sum := sha256.Sum256(content)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])

	tests := []struct {
		name    string
		handler http.HandlerFunc
		digest  string
	}{
		{name: "known length", handler: serveBlob(content), digest: wantDigest},
		{name: "chunked upstream", handler: serveChunked(content)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			up := newUpstream(t, tc.handler)
			p, base := startProxy(t, up.URL)
			p.AddFault(Fault{Name: "flip", Class: ClassBlobGet, Action: ActionCorruptBody, Times: 1})
			client := newClient(t)

			resp, body := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath})

			if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(content)) || len(body) != len(content) {
				t.Fatalf("status %d, length %d/%d, want %d", resp.StatusCode, resp.ContentLength, len(body), len(content))
			}

			if got := resp.Header.Get("Docker-Content-Digest"); got != tc.digest {
				t.Fatalf("Docker-Content-Digest %q, want %q", got, tc.digest)
			}

			mid := len(content) / 2
			if diff := diffOffsets(body, content); len(diff) != 1 || diff[0] != mid || body[mid] != content[mid]^0xff {
				t.Fatalf("differences at %v, want exactly offset %d", diff, mid)
			}

			if _, body := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath}); !bytes.Equal(body, content) {
				t.Fatal("fault applied more often than Times")
			}

			records := settled(t, p)
			if records[0].Fault != "flip" || records[0].RespBytes != int64(len(content)) || records[1].Fault != "" {
				t.Fatalf("records %+v", records)
			}
		})
	}
}

func TestBodyFaultSkipsResponsesWithoutBlob(t *testing.T) {
	t.Parallel()

	content := pattern(64)
	up := newUpstream(t, requireAuth(serveBlob(content)))
	p, base := startProxy(t, up.URL)
	p.AddFault(Fault{Name: "flip", PathContains: "/blobs/", Action: ActionCorruptBody, Times: 1})
	client := newClient(t)

	resp, body := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "authentication required") {
		t.Fatalf("challenge changed: %d %q", resp.StatusCode, body)
	}

	if resp, _ := mustSend(t, client, call{method: http.MethodHead, target: base + blobPath, header: authHeader()}); resp.ContentLength != int64(len(content)) {
		t.Fatalf("HEAD content length %d", resp.ContentLength)
	}

	if _, body := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath, header: authHeader()}); bytes.Equal(body, content) {
		t.Fatal("the first blob body was not corrupted")
	}

	if _, body := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath, header: authHeader()}); !bytes.Equal(body, content) {
		t.Fatal("the fault applied twice")
	}

	records := settled(t, p)

	faults := make([]string, 0, len(records))
	for _, rec := range records {
		faults = append(faults, rec.Fault)
	}

	if strings.Join(faults, ",") != ",,flip," {
		t.Fatalf("faults per record %q", faults)
	}
}

// TestRacingBodyFaults holds both upstream responses until both requests
// were admitted, so both start with the same candidate fault and the second
// response to arrive finds it used up.
func TestRacingBodyFaults(t *testing.T) {
	t.Parallel()

	const clients = 2

	content := pattern(1001)
	blob := serveBlob(content)
	allAdmitted := make(chan struct{})

	var arrivals atomic.Int32

	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if arrivals.Add(1) == clients {
			close(allAdmitted)
		}

		select {
		case <-allAdmitted:
			blob(w, r)
		case <-r.Context().Done():
		}
	})
	p, base := startProxy(t, up.URL)
	p.AddFault(Fault{Name: "corrupt-1", Class: ClassBlobGet, Action: ActionCorruptBody, Times: 1})
	p.AddFault(Fault{Name: "status", Class: ClassBlobGet, Status: http.StatusTeapot, Times: 1})
	p.AddFault(Fault{Name: "corrupt-2", Class: ClassBlobGet, Action: ActionCorruptBody, Times: 1})
	client := newClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	bodies := make([][]byte, clients)
	errs := make([]error, clients)

	var wg sync.WaitGroup

	for i := range clients {
		wg.Go(func() { bodies[i], errs[i] = get(ctx, client, base+blobPath) })
	}

	wg.Wait()

	mid := len(content) / 2

	for i := range clients {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}

		if diff := diffOffsets(bodies[i], content); len(bodies[i]) != len(content) || len(diff) != 1 || diff[0] != mid {
			t.Fatalf("body %d differs at %v, want exactly offset %d", i, diff, mid)
		}
	}

	if resp, _ := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath}); resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status %d, want the status fault that the racing exchanges skipped", resp.StatusCode)
	}

	if _, body := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath}); !bytes.Equal(body, content) {
		t.Fatal("a fault applied more often than Times")
	}

	applied := make([]string, 0, clients)
	for _, rec := range settled(t, p)[:clients] {
		applied = append(applied, rec.Fault)
	}

	slices.Sort(applied)

	if strings.Join(applied, ",") != "corrupt-1,corrupt-2" {
		t.Fatalf("faults applied to the racing exchanges %q", applied)
	}
}

func TestClearFaultsWhileWaitingForUpstream(t *testing.T) {
	t.Parallel()

	content := pattern(1001)
	blob := serveBlob(content)
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})

	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		default:
		}

		blob(w, r)
	})
	p, base := startProxy(t, up.URL)
	p.AddFault(Fault{Name: "old", Class: ClassBlobGet, Action: ActionCorruptBody})
	client := newClient(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var (
		body []byte
		err  error
	)

	done := make(chan struct{})

	go func() {
		defer close(done)

		body, err = get(ctx, client, base+blobPath)
	}()

	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("the request never reached the upstream")
	}

	p.ClearFaults()
	p.AddFault(Fault{Name: "new", Class: ClassBlobGet, Action: ActionCorruptBody, Times: 1})
	close(release)
	<-done

	if err != nil || !bytes.Equal(body, content) {
		t.Fatalf("exchange admitted before ClearFaults was changed (%v)", err)
	}

	if _, body := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath}); bytes.Equal(body, content) {
		t.Fatal("the fault added after ClearFaults did not reach the next request")
	}

	if records := settled(t, p); records[0].Fault != "" || records[1].Fault != "new" {
		t.Fatalf("records %+v", records)
	}
}

func get(ctx context.Context, client *http.Client, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	return io.ReadAll(resp.Body)
}

func TestFaultTruncateBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []byte
		chunked bool
	}{
		{name: "large", content: pattern(64<<10 + 1)},
		{name: "single byte", content: pattern(1)},
		{name: "chunked upstream", content: pattern(5000), chunked: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handler := serveBlob(tc.content)
			if tc.chunked {
				handler = serveChunked(tc.content)
			}

			up := newUpstream(t, handler)
			p, base := startProxy(t, up.URL)
			p.AddFault(Fault{Name: "cut", Method: http.MethodGet, Action: ActionTruncateBody, Times: 1})
			client := newClient(t)

			resp, body, err := send(t, client, call{method: http.MethodGet, target: base + blobPath})
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("read error %v, want io.ErrUnexpectedEOF", err)
			}

			half := len(tc.content) / 2
			if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(tc.content)) {
				t.Fatalf("status %d content length %d, want 200 and %d", resp.StatusCode, resp.ContentLength, len(tc.content))
			}

			if !bytes.Equal(body, tc.content[:half]) {
				t.Fatalf("received %d bytes, want the first %d unchanged", len(body), half)
			}

			records := settled(t, p)
			if len(records) != 1 {
				t.Fatalf("the client retried or the proxy recorded twice: %+v", records)
			}

			if rec := records[0]; rec.Fault != "cut" || rec.Status != http.StatusOK || rec.RespBytes != int64(half) {
				t.Fatalf("record %+v", rec)
			}

			if _, body := mustSend(t, client, call{method: http.MethodGet, target: base + blobPath}); !bytes.Equal(body, tc.content) {
				t.Fatal("fault applied more often than Times")
			}
		})
	}
}

func TestSetDown(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	p, base := startProxy(t, up.URL)
	client := newClient(t)

	p.SetDown(true)

	if resp, _ := mustSend(t, client, call{method: http.MethodGet, target: base + "/v2/"}); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d while down", resp.StatusCode)
	}

	if resp, body := mustSend(t, client, call{method: http.MethodHead, target: base + "/v2/"}); resp.StatusCode != http.StatusBadGateway || len(body) != 0 {
		t.Fatalf("HEAD while down: %d %q", resp.StatusCode, body)
	}

	if up.hits.Load() != 0 {
		t.Fatal("upstream contacted while down")
	}

	p.SetDown(false)

	if resp, _ := mustSend(t, client, call{method: http.MethodGet, target: base + "/v2/"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d after coming back", resp.StatusCode)
	}

	records := settled(t, p)
	if len(records) != 3 || records[0].Status != http.StatusBadGateway || records[0].Class != ClassPing || records[2].Status != http.StatusOK {
		t.Fatalf("records %+v", records)
	}
}

func TestUnreachableUpstream(t *testing.T) {
	t.Parallel()

	// An upstream that drops every connection before answering. Closing a
	// server instead would free its port for the parallel tests to reuse.
	gone, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = gone.Close() })

	go func() {
		for {
			conn, err := gone.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	target := "http://" + gone.Addr().String()

	p, base := startProxy(t, target)

	resp, body := mustSend(t, newClient(t), call{method: http.MethodGet, target: base + "/v2/"})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "regproxy: upstream") {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}

	if rec := settled(t, p)[0]; rec.Status != http.StatusBadGateway {
		t.Fatalf("record %+v", rec)
	}
}

func TestHostPreserved(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		seen  []string
		paths []string
	)

	up := newUpstream(t, func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		seen = append(seen, r.Host)
		paths = append(paths, r.URL.RequestURI())
	})
	p, base := startProxy(t, up.URL)
	client := newClient(t)

	mustSend(t, client, call{method: http.MethodGet, target: base + "/v2/org/schemas/tags/list?n=5"})
	mustSend(t, client, call{method: http.MethodGet, target: base + "/v2/", host: "registry.example:5000"})

	mu.Lock()
	defer mu.Unlock()

	if len(seen) != 2 || seen[0] != p.URLHost() || seen[1] != "registry.example:5000" {
		t.Fatalf("upstream saw hosts %q, want %q and registry.example:5000", seen, p.URLHost())
	}

	if paths[0] != "/v2/org/schemas/tags/list?n=5" || paths[1] != "/v2/" {
		t.Fatalf("upstream saw paths %q", paths)
	}
}

func TestLifecycle(t *testing.T) {
	t.Parallel()

	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})

	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}

	p := New(u)
	if p.URLHost() != "" {
		t.Fatal("URLHost set before Start")
	}

	base, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}

	if base != "http://"+p.URLHost() || !strings.HasPrefix(p.URLHost(), "127.0.0.1:") {
		t.Fatalf("base %q host %q", base, p.URLHost())
	}

	if _, err := p.Start(); err == nil {
		t.Fatal("second Start succeeded")
	}

	u.Host = "mutated.invalid"

	client := newClient(t)
	if resp, _ := mustSend(t, client, call{method: http.MethodGet, target: base + "/v2/"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy followed a mutation of the caller's URL: %d", resp.StatusCode)
	}

	p.Close()
	p.Close()

	if _, err := p.Start(); err == nil {
		t.Fatal("Start after Close succeeded")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}

	client.Transport.(*http.Transport).CloseIdleConnections()

	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()

		t.Fatal("closed proxy still answers")
	}
}

func TestWithTransport(t *testing.T) {
	t.Parallel()

	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "tls")
	}))
	t.Cleanup(up.Close)

	_, base := startProxy(t, up.URL, WithTransport(up.Client().Transport))

	if _, body := mustSend(t, newClient(t), call{method: http.MethodGet, target: base + "/v2/"}); string(body) != "tls" {
		t.Fatalf("body %q", body)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

type panickingTransport struct{}

func (panickingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("transport bug")
}

func TestWithErrorLog(t *testing.T) {
	t.Parallel()

	t.Run("handler panic is reported", func(t *testing.T) {
		t.Parallel()

		var logs lockedBuffer

		p, base := startProxy(t, "http://127.0.0.1:1", WithTransport(panickingTransport{}), WithErrorLog(log.New(&logs, "", 0)))

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v2/", nil)
		if err != nil {
			t.Fatal(err)
		}

		if resp, err := newClient(t).Do(req); err == nil {
			_ = resp.Body.Close()

			t.Fatalf("status %d from a panicking handler", resp.StatusCode)
		}

		// net/http logs the panic before it closes the connection, which is
		// what the client just observed.
		if got := logs.String(); !strings.Contains(got, "panic serving") || !strings.Contains(got, "transport bug") {
			t.Fatalf("server log %q, want the panic and its stack", got)
		}

		if records := settled(t, p); len(records) != 1 || records[0].Status != 0 {
			t.Fatalf("records %+v", records)
		}
	})

	t.Run("deliberate truncation is quiet", func(t *testing.T) {
		t.Parallel()

		var logs lockedBuffer

		up := newUpstream(t, serveBlob(pattern(4096)))
		p, base := startProxy(t, up.URL, WithErrorLog(log.New(&logs, "", 0)))
		p.AddFault(Fault{Name: "cut", Action: ActionTruncateBody})

		if _, _, err := send(t, newClient(t), call{method: http.MethodGet, target: base + blobPath}); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("read error %v, want io.ErrUnexpectedEOF", err)
		}

		settled(t, p)

		if got := logs.String(); got != "" {
			t.Fatalf("server logged a deliberate truncation: %q", got)
		}
	})
}

func TestAddFaultRejectsMistakes(t *testing.T) {
	t.Parallel()

	faults := map[string]Fault{
		"unknown action":  {Action: Action(42)},
		"negative action": {Action: Action(-1)},
		"negative times":  {Times: -1},
		"status too low":  {Status: 99},
		"informational":   {Status: http.StatusContinue},
		"status too high": {Status: 600},
	}

	for name, f := range faults {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if recover() == nil {
					t.Fatal("AddFault accepted an invalid fault")
				}
			}()

			New(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}).AddFault(f)
		})
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()

	content := pattern(2048)
	up := newUpstream(t, serveBlob(content))
	p, base := startProxy(t, up.URL)
	client := newClient(t)

	const workers, perWorker = 16, 20

	var wg sync.WaitGroup

	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				switch (w + i) % 4 {
				case 0:
					p.AddFault(Fault{Name: "x", Class: ClassOther, Status: http.StatusTeapot, Times: 1})
				case 1:
					_ = p.Stats()
				case 2:
					_ = p.Records()
				default:
					p.SetDown(false)
				}

				if err := fetch(client, base+blobPath, content); err != nil {
					t.Error(err)
				}
			}
		})
	}

	wg.Wait()
	p.ClearFaults()

	stats := settledStats(t, p)
	if stats.Total != workers*perWorker || stats.ByClass[ClassBlobGet] != workers*perWorker ||
		stats.BlobBytesOut != int64(workers*perWorker*len(content)) {
		t.Fatalf("stats %+v", stats)
	}
}

// fetch is safe to call from goroutines other than the test's own.
func fetch(client *http.Client, target string, want []byte) error {
	resp, err := client.Get(target)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
		return errors.New("unexpected response " + resp.Status)
	}

	return nil
}

type patternReader struct {
	off int64
}

func (r *patternReader) Read(p []byte) (int, error) {
	for i := range p {
		n := r.off + int64(i)
		p[i] = byte(n*7 + n/251)
	}

	r.off += int64(len(p))

	return len(p), nil
}

type streamResult struct {
	sum []byte
	err error
}

func TestStreamsLargeBody(t *testing.T) {
	const (
		size = 10 << 20
		head = 1 << 20
	)

	release := make(chan struct{})

	var releaseOnce sync.Once

	open := func() { releaseOnce.Do(func() { close(release) }) }

	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))

		src := io.LimitReader(&patternReader{}, size)
		if _, err := io.CopyN(w, src, head); err != nil {
			return
		}

		_ = http.NewResponseController(w).Flush()

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		_, _ = io.Copy(w, src)
	})
	t.Cleanup(open)

	p, base := startProxy(t, up.URL)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+blobPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	client := newClient(t)
	first := make(chan error, 1)
	rest := make(chan streamResult, 1)

	// A buffering proxy would not even send the response header while the
	// upstream holds back the rest, so the request itself runs under the
	// deadline too.
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			first <- err

			return
		}

		defer func() { _ = resp.Body.Close() }()

		h := sha256.New()

		_, err = io.CopyN(h, resp.Body, head)
		first <- err

		if err == nil {
			_, err = io.Copy(h, resp.Body)
			rest <- streamResult{sum: h.Sum(nil), err: err}
		}
	}()

	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		open()
		t.Fatal("the first MiB did not arrive while the upstream held back the rest: the proxy buffers bodies")
	}

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)
	open()

	var got streamResult

	select {
	case got = <-rest:
		if got.err != nil {
			t.Fatal(got.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the rest of the body did not arrive")
	}

	runtime.ReadMemStats(&after)

	want := sha256.New()
	if _, err := io.Copy(want, io.LimitReader(&patternReader{}, size)); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got.sum, want.Sum(nil)) {
		t.Fatal("streamed body differs from the upstream body")
	}

	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("allocated %d bytes while streaming %d bytes", allocated, size-head)

	if allocated > size/2 {
		t.Fatal("the proxy appears to buffer bodies")
	}

	if stats := settledStats(t, p); stats.BlobBytesOut != size {
		t.Fatalf("BlobBytesOut %d, want %d", stats.BlobBytesOut, size)
	}
}
