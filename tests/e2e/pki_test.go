//go:build e2e

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// credential is a registry user of the auth profile. Passwords are random
// per run and must never appear in any output a test asserts on.
type credential struct {
	Username string
	Password string
}

// pki holds the run's test CA, the server certificate both TLS registries
// present, and their htpasswd users.
type pki struct {
	// CAFile is the PEM CA certificate; give it to clients as ca_file.
	CAFile string
	pool   *x509.CertPool
	users  map[string]credential
}

// newPKI writes certs/{ca.crt,server.crt,server.key}, auth/htpasswd and
// auth2/htpasswd below dir. The files are world-readable on purpose: a
// Docker daemon with user-namespace remapping runs the registry as an
// unprivileged host user that must read them. The key protects nothing but
// throwaway local containers.
func newPKI(dir string) (*pki, error) {
	certs := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certs, 0o755); err != nil {
		return nil, fmt.Errorf("create certs directory: %w", err)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "schepherd e2e test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(48 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server key: %w", err)
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(48 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", serviceAuth, serviceAuth2},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create server certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return nil, fmt.Errorf("encode server key: %w", err)
	}

	p := &pki{CAFile: filepath.Join(certs, "ca.crt"), pool: x509.NewCertPool(), users: map[string]credential{}}
	p.pool.AddCert(caCert)

	files := map[string][]byte{
		p.CAFile:                           pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		filepath.Join(certs, "server.crt"): pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		filepath.Join(certs, "server.key"): pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}

	for path, data := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}

		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
	}

	for service, sub := range map[string]string{serviceAuth: "auth", serviceAuth2: "auth2"} {
		user := credential{Username: "e2e-" + sub + "-user", Password: randomHex(16)}

		hash, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash password: %w", err)
		}

		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", sub, err)
		}

		line := user.Username + ":" + string(hash) + "\n"
		if err := os.WriteFile(filepath.Join(dir, sub, "htpasswd"), []byte(line), 0o644); err != nil {
			return nil, fmt.Errorf("write htpasswd: %w", err)
		}

		p.users[service] = user
	}

	return p, nil
}

func (p *pki) tlsConfig() *tls.Config {
	return &tls.Config{RootCAs: p.pool, MinVersion: tls.VersionTLS12}
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}

	return n
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
