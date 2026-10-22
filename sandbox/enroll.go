package sandbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Elchi-dev/kapelo-sandbox/protocol"
)

// Enroll trades a one time token from Kapelo for this sandbox's
// certificate, and writes the configuration and identity into the data
// folder. caFile pins Kapelo's CA for the request itself; empty trusts
// the system roots.
func Enroll(ctx context.Context, c *Config, token, caFile, version string) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	keyPEM, csrPEM, err := newKeyAndCSR(c.Name)
	if err != nil {
		return "", err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return "", err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return "", fmt.Errorf("enroll: %s holds no certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return "", err
	}
	body, _ := json.Marshal(protocol.EnrollRequest{Token: strings.TrimSpace(token), Name: c.Name, CSR: string(csrPEM),
		Version: version, CPUs: float64(runtime.NumCPU()), MemoryMB: memoryMB(), DiskGB: diskGB(c.DataDir), Runtime: containerRuntime()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+c.Server+"/agent/enroll", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment}}
	resp, err := cl.Do(req)
	if err != nil {
		return "", fmt.Errorf("enroll: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("enroll: Kapelo answered %s: %s", resp.Status, bytes.TrimSpace(rb))
	}
	var out protocol.EnrollResponse
	if err := json.Unmarshal(rb, &out); err != nil || out.SandboxID == "" {
		return "", errors.New("enroll: unexpected answer from Kapelo")
	}
	ident, _ := json.Marshal(map[string]string{"sandbox_id": out.SandboxID})
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{fileKey, keyPEM, 0o600}, {fileCert, []byte(out.Cert), 0o644}, {fileCA, []byte(out.CA), 0o644},
		{fileCmd, []byte(out.CommandKey), 0o644}, {fileIdent, ident, 0o644},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(c.DataDir, f.name), f.data, f.mode); err != nil {
			return "", err
		}
	}
	_ = os.Remove(filepath.Join(c.DataDir, filePair))
	if err := c.Save(); err != nil {
		return "", err
	}
	return out.SandboxID, nil
}

// memoryMB is the container's memory limit, or the machine's memory.
func memoryMB() int {
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			return int(n >> 20)
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "MemTotal:" {
				if kb, err := strconv.Atoi(f[1]); err == nil {
					return kb >> 10
				}
			}
		}
	}
	return 0
}

func diskGB(dir string) int {
	return int(diskFree(dir))
}

func diskFree(dir string) float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
}

// containerRuntime tells gVisor apart from an ordinary container, as far
// as the inside can see.
func containerRuntime() string {
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil && strings.Contains(string(b), "runsc") {
		return "runsc"
	}
	if b, err := os.ReadFile("/proc/version"); err == nil && strings.Contains(string(b), "gVisor") {
		return "runsc"
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	return "unknown"
}
