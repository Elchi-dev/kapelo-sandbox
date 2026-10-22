package sandbox

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Elchi-dev/kapelo-sandbox/link"
	"github.com/Elchi-dev/kapelo-sandbox/protocol"
)

// fakeKapelo is the part of Kapelo a sandbox talks to: enrollment and the
// link, with a CA of its own and a command signing key.
type fakeKapelo struct {
	t        *testing.T
	addr     string
	caFile   string
	caKey    *ecdsa.PrivateKey
	caCert   *x509.Certificate
	caPEM    []byte
	signKey  ed25519.PrivateKey
	sessions chan *yamux.Session
	tokens   map[string]bool
}

func newFakeKapelo(t *testing.T) *fakeKapelo {
	t.Helper()
	k := &fakeKapelo{t: t, sessions: make(chan *yamux.Session, 4), tokens: map[string]bool{"kpe_good": true}}
	k.caKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.caKey.PublicKey, k.caKey)
	k.caCert, _ = x509.ParseCertificate(der)
	k.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	k.caFile = filepath.Join(t.TempDir(), "ca.crt")
	os.WriteFile(k.caFile, k.caPEM, 0o644)
	_, k.signKey, _ = ed25519.GenerateKey(rand.Reader)

	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	srvDER, _ := x509.CreateCertificate(rand.Reader, srvTmpl, k.caCert, &srvKey.PublicKey, k.caKey)
	pool := x509.NewCertPool()
	pool.AddCert(k.caCert)
	tcfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{srvDER}, PrivateKey: srvKey}},
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tcfg)
	if err != nil {
		t.Fatal(err)
	}
	k.addr = ln.Addr().String()
	m := http.NewServeMux()
	m.HandleFunc("POST /agent/enroll", k.enroll)
	m.HandleFunc("GET /agent/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "certificate required", http.StatusUnauthorized)
			return
		}
		s, err := link.Upgrade(w, r)
		if err == nil {
			k.sessions <- s
		}
	})
	srv := &http.Server{Handler: m}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return k
}

func (k *fakeKapelo) enroll(w http.ResponseWriter, r *http.Request) {
	var req protocol.EnrollRequest
	json.NewDecoder(r.Body).Decode(&req)
	if !k.tokens[req.Token] {
		http.Error(w, "unknown token", http.StatusForbidden)
		return
	}
	delete(k.tokens, req.Token)
	blk, _ := pem.Decode([]byte(req.CSR))
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "sb_test"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, k.caCert, csr.PublicKey, k.caKey)
	pub, _ := x509.MarshalPKIXPublicKey(k.signKey.Public())
	json.NewEncoder(w).Encode(protocol.EnrollResponse{SandboxID: "sb_test", Cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		CA: string(k.caPEM), CommandKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))})
}

// call signs a command and sends it over the link.
func call(t *testing.T, cl *http.Client, key ed25519.PrivateKey, kind string, payload, out any) int {
	t.Helper()
	sig, err := protocol.Sign(key, "sb_test", kind, payload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(sig)
	resp, err := cl.Post("http://link/cmd", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("%s: %v: %s", kind, err, body)
		}
	}
	if resp.StatusCode != http.StatusOK && out != nil {
		if e, ok := out.(*string); ok {
			*e = string(body)
		}
	}
	return resp.StatusCode
}

func TestSandboxEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	k := newFakeKapelo(t)
	data := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	off := false
	cfg := &Config{Name: "box", Server: k.addr, DataDir: data, Postgres: &off}
	if _, err := Enroll(ctx, cfg, "kpe_wrong", k.caFile, "test"); err == nil || !strings.Contains(err.Error(), "unknown token") {
		t.Fatalf("a wrong token: %v", err)
	}
	id, err := Enroll(ctx, cfg, "kpe_good", k.caFile, "test")
	if err != nil || id != "sb_test" {
		t.Fatalf("enroll: %s %v", id, err)
	}
	if st, _ := os.Stat(filepath.Join(data, fileKey)); st.Mode().Perm() != 0o600 {
		t.Errorf("the private key is readable by others: %v", st.Mode())
	}
	loaded, err := LoadConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := loaded.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	sb := &Sandbox{Cfg: loaded, ID: ident, Version: "test"}
	done := make(chan error, 1)
	go func() { done <- sb.Run(ctx) }()
	var sess *yamux.Session
	select {
	case sess = <-k.sessions:
	case <-time.After(10 * time.Second):
		t.Fatal("the sandbox did not connect")
	}
	cl := link.Client(sess)
	key := k.signKey

	var head map[string]string
	if code := call(t, cl, key, protocol.ProjectEnsure, protocol.ProjectRequest{Project: "shop"}, &head); code != 200 || len(head["head"]) != 40 {
		t.Fatalf("ensure: %d %v", code, head)
	}
	if code := call(t, cl, key, protocol.Write, protocol.WriteRequest{Project: "shop", Path: "index.html", Content: "<h1>shop</h1>\n"}, nil); code != 200 {
		t.Fatalf("write: %d", code)
	}
	var ex protocol.ExecResult
	call(t, cl, key, protocol.Exec, protocol.ExecRequest{Project: "shop", Command: "git add -A && git commit -qm 'Add index' && git log --format=%an -1 && echo $HOME"}, &ex)
	if ex.ExitCode != 0 || !strings.Contains(ex.Output, "Kapelo\n"+filepath.Join(data, "home")) {
		t.Fatalf("exec: %+v", ex)
	}

	// An app the agent starts is reachable through the preview path.
	if _, err := exec.LookPath("python3"); err == nil {
		port := freePort(t)
		var bg protocol.ExecResult
		call(t, cl, key, protocol.Exec, protocol.ExecRequest{Project: "shop", Command: fmt.Sprintf("python3 -m http.server %d --bind 127.0.0.1", port), Background: true}, &bg)
		resp, err := cl.Get(fmt.Sprintf("http://link/preview/%d/index.html", port))
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(page) != "<h1>shop</h1>\n" {
			t.Errorf("preview: %d %q", resp.StatusCode, page)
		}
		call(t, cl, key, protocol.Process, protocol.ProcessRequest{Action: "stop", ProcessID: bg.ProcessID}, nil)
	}

	// A bundle of main goes out, and a bundle with more work comes back in.
	sig, _ := protocol.Sign(key, "sb_test", protocol.ProjectBundle, protocol.ProjectRequest{Project: "shop", Branch: "main"}, time.Minute)
	b, _ := json.Marshal(sig)
	resp, err := cl.Post("http://link/cmd", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.HasPrefix(bundle, []byte("# v2 git bundle")) && !bytes.HasPrefix(bundle, []byte("# v3 git bundle")) {
		t.Fatalf("bundle: %d %.40q", resp.StatusCode, bundle)
	}
	work := t.TempDir()
	bfile := filepath.Join(work, "in.bundle")
	os.WriteFile(bfile, bundle, 0o644)
	clone := filepath.Join(work, "clone")
	runGit(t, work, "clone", "-q", "--branch", "main", bfile, clone)
	os.WriteFile(filepath.Join(clone, "about.html"), []byte("about\n"), 0o644)
	runGit(t, clone, "add", "-A")
	runGit(t, clone, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "About")
	runGit(t, clone, "bundle", "create", filepath.Join(work, "back.bundle"), "main")
	back, _ := os.ReadFile(filepath.Join(work, "back.bundle"))
	sum := sha256.Sum256(back)
	sig, _ = protocol.Sign(key, "sb_test", protocol.ProjectSeed, protocol.ProjectRequest{Project: "shop", Branch: "main",
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(back))}, time.Minute)
	sb64, _ := json.Marshal(sig)
	req, _ := http.NewRequest(http.MethodPost, "http://link/cmd-data", bytes.NewReader(back))
	req.Header.Set("X-Command", base64.StdEncoding.EncodeToString(sb64))
	resp, err = cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("seed: %d", resp.StatusCode)
	}
	var rd protocol.ReadResult
	call(t, cl, key, protocol.Read, protocol.ReadRequest{Project: "shop", Path: "about.html"}, &rd)
	if rd.Content != "     1\tabout\n" {
		t.Errorf("after seed: %q", rd.Content)
	}

	// Commands signed by anyone else, or replayed, are refused.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if code := call(t, cl, other, protocol.Exec, protocol.ExecRequest{Project: "shop", Command: "touch pwned"}, nil); code != http.StatusForbidden {
		t.Errorf("forged command: %d", code)
	}
	replay, _ := protocol.Sign(key, "sb_test", protocol.Exec, protocol.ExecRequest{Project: "shop", Command: "echo once >> count"}, time.Minute)
	rb, _ := json.Marshal(replay)
	for i, want := range []int{200, 403} {
		resp, _ := cl.Post("http://link/cmd", "application/json", bytes.NewReader(rb))
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("send %d of one command: %d", i+1, resp.StatusCode)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "projects", "shop", "pwned")); err == nil {
		t.Error("the forged command ran")
	}

	// A refusal comes back as 422 with a reason the agent can act on.
	var msg string
	if code := call(t, cl, key, protocol.Edit, protocol.EditRequest{Project: "shop", Path: "index.html", Old: "nope", New: "x"}, &msg); code != 422 || !strings.Contains(msg, "does not occur") {
		t.Errorf("refusal: %d %s", code, msg)
	}

	// The owner pauses: nothing runs until resumed.
	sb.Pause()
	if code := call(t, cl, key, protocol.Exec, protocol.ExecRequest{Project: "shop", Command: "true"}, nil); code != http.StatusLocked {
		t.Errorf("paused: %d", code)
	}
	var st protocol.StatusResult
	if code := call(t, cl, key, protocol.Status, nil, &st); code != 200 || !st.Paused || len(st.Projects) != 1 {
		t.Errorf("status while paused: %d %+v", code, st)
	}
	sb.Resume()
	if code := call(t, cl, key, protocol.Exec, protocol.ExecRequest{Project: "shop", Command: "true"}, &ex); code != 200 {
		t.Errorf("resumed: %d", code)
	}
	cancel()
	<-done
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
