package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ovineko/schepherd/internal/fault"
	"github.com/ovineko/schepherd/internal/publisher/upstream/tomlfile"
	"github.com/ovineko/schepherd/internal/registry"
)

const maxRegistryConfigBytes = 1 << 20

type registriesDoc struct {
	Registries map[string]registryDoc `toml:"registries"`
}

type registryDoc struct {
	CAFile          string `toml:"ca_file"`
	CredentialsFile string `toml:"credentials_file"`
	PlainHTTP       bool   `toml:"plain_http"`
}

// loadRegistries reads the --registry-config file: only
// [registries."host[:port]"] and [registries."host[:port]/path"] tables with
// plain_http, ca_file and credentials_file, the same shape as in the client
// configuration; the longest key matching a repository applies on its own.
// Relative file paths are resolved against the file's directory. An empty
// path means no per-host settings: HTTPS, system roots and the standard
// Docker credential discovery. Every error is fault.Usage.
func loadRegistries(path string) (map[string]registry.HostConfig, error) {
	hosts := map[string]registry.HostConfig{}
	if path == "" {
		return hosts, nil
	}

	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fault.Wrap(fault.Usage, err, "registry configuration")
	}

	var doc registriesDoc
	if err := tomlfile.Decode(path, maxRegistryConfigBytes, &doc); err != nil {
		return nil, fault.Wrap(fault.Usage, err, "registry configuration")
	}

	dir := filepath.Dir(path)

	for host, r := range doc.Registries {
		if !isRegistryKey(host) {
			return nil, fault.New(fault.Usage, "registry configuration %s: %q is not a registry host[:port] or host[:port]/path", path, host)
		}

		cfg := registry.HostConfig{PlainHTTP: r.PlainHTTP}

		if cfg.CAFile, err = regularFile(dir, r.CAFile); err != nil {
			return nil, fault.Wrap(fault.Usage, err, "registry configuration %s: registries.%q.ca_file", path, host)
		}

		if cfg.CredentialsFile, err = regularFile(dir, r.CredentialsFile); err != nil {
			return nil, fault.Wrap(fault.Usage, err, "registry configuration %s: registries.%q.credentials_file", path, host)
		}

		hosts[host] = cfg
	}

	return hosts, nil
}

func isRegistryKey(key string) bool {
	if parsed, err := registry.ParseRepository(key + "/x"); err == nil && parsed.Host == key {
		return true
	}

	parsed, err := registry.ParseRepository(key)

	return err == nil && parsed.String() == key
}

func regularFile(dir, name string) (string, error) {
	if name == "" {
		return "", nil
	}

	if !filepath.IsAbs(name) {
		name = filepath.Join(dir, name)
	}

	info, err := os.Stat(name)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", name)
	}

	return name, nil
}
