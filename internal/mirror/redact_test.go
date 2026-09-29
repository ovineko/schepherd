package mirror

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ovineko/schepherd/internal/fault"
)

// signedQuery is the query of a presigned storage URL: an S3 signature and
// credential scope plus an Azure SAS signature.
const signedQuery = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAEXAMPLE%2F20260925%2Fus-east-1%2Fs3%2Faws4_request" +
	"&X-Amz-Signature=SECRETSIGNATURE123&sig=SECRETSAS456&se=2099-01-01"

// TestRunDoesNotShowSignedStorageURLs has the source registry redirect a
// schema payload, which only the graph copy downloads, to a presigned
// storage URL whose hop then fails.
func TestRunDoesNotShowSignedStorageURLs(t *testing.T) {
	storages := []struct {
		base     func(t *testing.T) string
		name     string
		fragment string
	}{
		{name: "connection refused", fragment: "network error", base: func(t *testing.T) string {
			t.Helper()

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}

			addr := ln.Addr().String()
			if err := ln.Close(); err != nil {
				t.Fatal(err)
			}

			return "http://" + addr
		}},
		{name: "403", fragment: "access denied (403)", base: func(t *testing.T) string {
			t.Helper()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "<Error><Code>SignatureDoesNotMatch</Code><Query>"+r.URL.RawQuery+"</Query></Error>")
			}))
			t.Cleanup(srv.Close)

			return srv.URL
		}},
	}

	for _, storage := range storages {
		t.Run(storage.name, func(t *testing.T) {
			src, dst := newFakeRegistry(), newFakeRegistry()
			snap := firstSnapshot(t, src)
			payload := snap.payloads["beta"]
			storageURL := storage.base(t) + "/bucket/" + payload
			src.setRedirect(payload, storageURL+"?"+signedQuery)
			repos := openRepos(t, src, dst)

			ctx, cancel := withTimeout(t)
			defer cancel()

			_, err := Run(ctx, repos[0], repos[1], snap.catalog, runOptions())
			if fault.KindOf(err) != fault.Registry {
				t.Fatalf("mirror through a failing storage hop = %v (kind %s), want a registry error", err, fault.KindOf(err))
			}

			msg := err.Error()
			for _, want := range []string{storage.fragment, `"` + storageURL + `"`} {
				if !strings.Contains(msg, want) {
					t.Errorf("error does not contain %q: %s", want, msg)
				}
			}

			for _, leak := range []string{"?", "#", "SECRET", "X-Amz", "AKIAEXAMPLE", "sig=", "se=2099"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error shows %q from the storage URL query: %s", leak, msg)
				}
			}

			assertNoCatalogTag(t, dst, snap)
		})
	}
}
