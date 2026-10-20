package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPublishedParserAnonymousRetrieval(t *testing.T) {
	for _, scenario := range []string{
		"valid", "valid redirect", "corrupt redirect", "corrupt manifest", "missing manifest", "missing payload", "corrupt payload", "short payload",
		"wrong layer digest", "wrong layer size", "compressed layer", "extra layer", "no layer",
		"wrong config type", "wrong config digest", "wrong config size", "missing config", "corrupt config",
		"wrong artifact type", "wrong manifest type", "wrong schema version", "wrong version", "private",
	} {
		t.Run(scenario, func(t *testing.T) {
			module := []byte{0, 'a', 's', 'm', 1, 0, 0, 0, 0}
			metadata, body, err := parserArtifact(module, "1.2.3")
			if err != nil {
				t.Fatal(err)
			}
			var manifest ocispec.Manifest
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "wrong layer digest":
				manifest.Layers[0].Digest = parserDescriptor([]byte("other"), "application/wasm").Digest
			case "wrong layer size":
				manifest.Layers[0].Size++
			case "compressed layer":
				manifest.Layers[0].MediaType = "application/wasm+gzip"
			case "extra layer":
				manifest.Layers = append(manifest.Layers, manifest.Layers[0])
			case "no layer":
				manifest.Layers = nil
			case "wrong config type":
				manifest.Config.MediaType = ocispec.MediaTypeImageConfig
			case "wrong config digest":
				manifest.Config.Digest = manifest.Layers[0].Digest
			case "wrong config size":
				manifest.Config.Size++
			case "wrong artifact type":
				manifest.ArtifactType = "application/other"
			case "wrong manifest type":
				manifest.MediaType = ocispec.MediaTypeImageIndex
			case "wrong schema version":
				manifest.SchemaVersion = 1
			case "wrong version":
				manifest.Annotations[ocispec.AnnotationVersion] = "9.9.9"
			}
			body, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			metadata.OCI.Digest = parserHash(body)
			if scenario == "corrupt manifest" {
				body = append(body, '\n')
			}
			var tokenRequests, payloadRequests, configRequests atomic.Int32
			var leakedCredential atomic.Bool
			var redirectRequests atomic.Int32
			storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirectRequests.Add(1)
				if r.Header.Get("Authorization") != "" {
					leakedCredential.Store(true)
				}
				data := bytes.Clone(module)
				if scenario == "corrupt redirect" {
					data[len(data)-1]++
				}
				_, _ = w.Write(data)
			}))
			t.Cleanup(storage.Close)
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokenRequests.Add(1)
					if r.Header.Get("Authorization") != "" {
						leakedCredential.Store(true)
						_, _ = fmt.Fprint(w, `{"token":"push-token"}`)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, `{"token":"anonymous-token"}`)
					return
				}
				got := r.Header.Get("Authorization")
				if (got != "Bearer anonymous-token" && got != "Bearer push-token") || (scenario == "private" && got != "Bearer push-token") {
					if got != "" && got != "Bearer anonymous-token" {
						leakedCredential.Store(true)
					}
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="fixture",scope="repository:org/parsers:pull"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				var data []byte
				switch r.URL.Path {
				case "/v2/org/parsers/manifests/" + metadata.OCI.Digest:
					if scenario == "missing manifest" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
					w.Header().Set("Docker-Content-Digest", metadata.OCI.Digest)
					data = body
				case "/v2/org/parsers/blobs/" + parserHash([]byte("{}")):
					configRequests.Add(1)
					if scenario == "missing config" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					data = []byte("{}")
					if scenario == "corrupt config" {
						data = []byte("[]")
					}
				case "/v2/org/parsers/blobs/" + metadata.Hash:
					payloadRequests.Add(1)
					if scenario == "valid redirect" || scenario == "corrupt redirect" {
						http.Redirect(w, r, storage.URL+"/module?signature=not-a-credential", http.StatusTemporaryRedirect)
						return
					}
					if scenario == "missing payload" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					data = bytes.Clone(module)
					if scenario == "corrupt payload" {
						data[len(data)-1]++
					}
					if scenario == "short payload" {
						data = data[:len(data)-1]
					}
				default:
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				_, _ = w.Write(data)
			}))
			t.Cleanup(server.Close)
			host := strings.TrimPrefix(server.URL, "http://")
			metadata.OCI.Ref = host + "/org/parsers"
			docker := t.TempDir()
			auth := base64.StdEncoding.EncodeToString([]byte("push-user:push-secret"))
			if err := os.WriteFile(filepath.Join(docker, "config.json"), []byte(`{"auths":{"`+host+`":{"auth":"`+auth+`"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DOCKER_CONFIG", docker)
			t.Setenv("GITHUB_TOKEN", "must-not-be-used")
			err = verifyPublishedParsers(context.Background(), metadata, true)
			if scenario == "valid" || scenario == "valid redirect" {
				if err != nil {
					t.Fatal(err)
				}
				if payloadRequests.Load() != 1 || configRequests.Load() != 1 {
					t.Fatal("successful verification did not retrieve both payload and config")
				}
				if scenario == "valid redirect" && redirectRequests.Load() != 1 {
					t.Fatal("successful verification did not retrieve redirected payload")
				}
			} else if err == nil {
				t.Fatal("accepted invalid or non-public parser artifact")
			}
			if tokenRequests.Load() == 0 || leakedCredential.Load() {
				t.Fatal("verification did not authenticate anonymously")
			}
		})
	}
}
