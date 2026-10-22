// Package sandbox is the Kapelo sandbox: one long running container on
// somebody's own server or computer, coupled to Kapelo. It keeps one
// outbound connection to Kapelo and gives Kapelo's agents tools in it:
// running commands, reading and writing files, searching, and reaching
// the apps they start. Every command must be signed by Kapelo for this
// sandbox. Projects live in the data volume and stay between sessions.
package sandbox

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is <data>/sandbox.json, written at enrollment.
type Config struct {
	Name    string `json:"name"`
	Server  string `json:"server"` // host:port of Kapelo's sandbox endpoint
	DataDir string `json:"-"`
	// Postgres runs a database for the projects, reachable as
	// DATABASE_URL in every command. On by default.
	Postgres *bool `json:"postgres,omitempty"`
}

// DefaultServer is Kapelo's sandbox endpoint.
const DefaultServer = "agent.kapelo.app:443"

func (c *Config) PostgresOn() bool { return c.Postgres == nil || *c.Postgres }

func (c *Config) Projects() string { return filepath.Join(c.DataDir, "projects") }
func (c *Config) Home() string     { return filepath.Join(c.DataDir, "home") }

func (c *Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.Name) == "" {
		errs = append(errs, errors.New("name is empty"))
	}
	if !strings.Contains(c.Server, ":") {
		errs = append(errs, fmt.Errorf("server %q needs host:port", c.Server))
	}
	return errors.Join(errs...)
}

const (
	fileConfig = "sandbox.json"
	fileKey    = "sandbox.key"
	fileCert   = "sandbox.crt"
	filePair   = "sandbox.pem" // a renewed certificate with its key, replaced at once
	fileCA     = "ca.crt"
	fileCmd    = "commands.pub"
	fileIdent  = "identity.json"
	fileSocket = "sandbox.sock"
)

// Enrolled reports whether the data folder holds an identity.
func Enrolled(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, fileIdent))
	return err == nil
}

func LoadConfig(dataDir string) (*Config, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, fileConfig))
	if err != nil {
		return nil, fmt.Errorf("sandbox: %w; enroll first", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("sandbox: %s: %w", fileConfig, err)
	}
	c.DataDir = dataDir
	return &c, c.Validate()
}

func (c *Config) Save() error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(c.DataDir, fileConfig), b, 0o600)
}

// Identity is what enrollment gave this sandbox.
type Identity struct {
	SandboxID  string
	TLS        *tls.Config
	CommandKey ed25519.PublicKey
}

func (c *Config) LoadIdentity() (*Identity, error) {
	read := func(n string) ([]byte, error) { return os.ReadFile(filepath.Join(c.DataDir, n)) }
	idb, err := read(fileIdent)
	if err != nil {
		return nil, fmt.Errorf("sandbox: not enrolled (%w)", err)
	}
	var id struct {
		SandboxID string `json:"sandbox_id"`
	}
	if err := json.Unmarshal(idb, &id); err != nil || id.SandboxID == "" {
		return nil, fmt.Errorf("sandbox: %s is damaged; enroll again", fileIdent)
	}
	certPEM, err1 := read(fileCert)
	keyPEM, err2 := read(fileKey)
	if pair, err := read(filePair); err == nil {
		certPEM, keyPEM, err1, err2 = pair, pair, nil, nil
	}
	caPEM, err3 := read(fileCA)
	cmdPEM, err4 := read(fileCmd)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("sandbox: certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("sandbox: ca.crt holds no certificate")
	}
	host := c.Server
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	blk, _ := pem.Decode(cmdPEM)
	if blk == nil {
		return nil, errors.New("sandbox: commands.pub is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("sandbox: commands.pub: %w", err)
	}
	ek, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("sandbox: commands.pub is not ed25519")
	}
	return &Identity{
		SandboxID: id.SandboxID,
		TLS: &tls.Config{
			Certificates: []tls.Certificate{pair},
			RootCAs:      pool, // pinned: the system roots are not trusted for the link
			ServerName:   host,
			MinVersion:   tls.VersionTLS13,
		},
		CommandKey: ek,
	}, nil
}

// newKeyAndCSR makes the sandbox's key pair; the private key never leaves
// the data folder.
func newKeyAndCSR(name string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}
