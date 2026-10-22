package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Elchi-dev/kapelo-sandbox/link"
	"github.com/Elchi-dev/kapelo-sandbox/pgdev"
	"github.com/Elchi-dev/kapelo-sandbox/protocol"
)

// Sandbox is the running daemon.
type Sandbox struct {
	Cfg     *Config
	ID      *Identity
	Version string
	// RenewBefore overrides how long before expiry the certificate is
	// renewed; zero means a month.
	RenewBefore time.Duration
	// PostgresPort is where the projects' database listens; 5432 when
	// empty.
	PostgresPort string

	tools    *Tools
	projects *Projects
	verifier *protocol.Verifier
	db       *pgdev.Server

	idMu   sync.Mutex
	linkUp chan struct{}

	mu      sync.Mutex
	session *yamux.Session
	paused  bool
	online  time.Time
	lastErr string
	events  []string
}

func (s *Sandbox) event(format string, args ...any) {
	line := time.Now().UTC().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...)
	slog.Info(fmt.Sprintf(format, args...))
	s.mu.Lock()
	s.events = append(s.events, line)
	if len(s.events) > 200 {
		s.events = s.events[len(s.events)-200:]
	}
	s.mu.Unlock()
}

// env is what every command and git call gets on top of the container's
// environment.
func (s *Sandbox) env() []string {
	env := []string{"HOME=" + s.Cfg.Home(), "GIT_CONFIG_GLOBAL=" + filepath.Join(s.Cfg.Home(), ".gitconfig"),
		"PATH=" + filepath.Join(s.Cfg.Home(), ".local", "bin") + ":" + os.Getenv("PATH")}
	if s.db != nil {
		env = append(env, "DATABASE_URL="+s.db.URL)
	}
	return env
}

// Run keeps the link up until ctx ends.
func (s *Sandbox) Run(ctx context.Context) error {
	for _, d := range []string{s.Cfg.Projects(), s.Cfg.Home()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(s.Cfg.Home(), ".gitconfig"), []byte(gitConfig), 0o644); err != nil {
		return err
	}
	s.tools = &Tools{Projects: s.Cfg.Projects(), Home: s.Cfg.Home(), Env: s.env}
	s.projects = &Projects{Root: s.Cfg.Projects(), Env: s.env}
	s.verifier = &protocol.Verifier{Key: s.identity().CommandKey, SandboxID: s.identity().SandboxID}
	if s.Cfg.PostgresOn() && pgdev.Available() {
		port := s.PostgresPort
		if port == "" {
			port = "5432"
		}
		db, err := pgdev.Persistent(ctx, filepath.Join(s.Cfg.DataDir, "postgres"), port)
		if err != nil {
			s.event("PostgreSQL did not start, commands run without DATABASE_URL: %v", err)
		} else {
			s.db = db
			defer db.Stop()
			s.event("PostgreSQL for the projects at %s", db.URL)
		}
	}
	ctl, err := s.serveControl()
	if err != nil {
		return err
	}
	defer ctl.Close()
	s.event("sandbox %s started as %s", s.Version, s.ID.SandboxID)

	s.linkUp = make(chan struct{}, 1)
	go s.renewLoop(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := s.connectOnce(ctx)
		if ctx.Err() != nil {
			break
		}
		s.mu.Lock()
		if err != nil {
			s.lastErr = err.Error()
		}
		s.mu.Unlock()
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		s.event("link down: %v; trying again in %s", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	s.tools.StopAll()
	return nil
}

func (s *Sandbox) identity() *Identity {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	return s.ID
}

func (s *Sandbox) connectOnce(ctx context.Context) error {
	sess, err := link.Dial(ctx, s.Cfg.Server, s.identity().TLS)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.session = sess
	s.online = time.Now()
	s.lastErr = ""
	s.mu.Unlock()
	s.event("link up to %s", s.Cfg.Server)
	select {
	case s.linkUp <- struct{}{}:
	default:
	}
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	err = link.Serve(sess, s.Handler())
	s.mu.Lock()
	s.session = nil
	s.mu.Unlock()
	if err == nil {
		err = errors.New("closed by Kapelo")
	}
	return err
}

// Handler is what Kapelo may call on the link.
func (s *Sandbox) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cmd", s.command)
	mux.HandleFunc("POST /cmd-data", s.commandWithData)
	mux.HandleFunc("/preview/{port}/{rest...}", s.servePreview)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// fail answers a tool error: refusals are the agent's to fix (422), the
// rest the sandbox's (500).
func fail(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if errors.Is(err, ErrRefused) {
		code = http.StatusUnprocessableEntity
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Sandbox) verify(w http.ResponseWriter, sig *protocol.Signed) (*protocol.Envelope, bool) {
	env, err := s.verifier.Verify(sig)
	if err != nil {
		s.event("refused a command: %v", err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return nil, false
	}
	s.mu.Lock()
	paused := s.paused
	s.mu.Unlock()
	if paused && env.Kind != protocol.Status {
		http.Error(w, "the sandbox is paused by its owner", http.StatusLocked)
		return nil, false
	}
	return env, true
}

func decode[T any](w http.ResponseWriter, env *protocol.Envelope) (T, bool) {
	var v T
	if err := json.Unmarshal(env.Payload, &v); err != nil {
		http.Error(w, "the payload of "+env.Kind+" is not valid: "+err.Error(), http.StatusBadRequest)
		return v, false
	}
	return v, true
}

func (s *Sandbox) command(w http.ResponseWriter, r *http.Request) {
	var sig protocol.Signed
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&sig); err != nil {
		http.Error(w, "bad command", http.StatusBadRequest)
		return
	}
	env, ok := s.verify(w, &sig)
	if !ok {
		return
	}
	ctx := r.Context()
	switch env.Kind {
	case protocol.Status:
		writeJSON(w, s.Status())
	case protocol.Exec:
		req, ok := decode[protocol.ExecRequest](w, env)
		if !ok {
			return
		}
		res, err := s.tools.Exec(ctx, req)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, res)
	case protocol.Process:
		req, ok := decode[protocol.ProcessRequest](w, env)
		if !ok {
			return
		}
		res, err := s.tools.Process(req)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, res)
	case protocol.Read:
		req, ok := decode[protocol.ReadRequest](w, env)
		if !ok {
			return
		}
		res, err := s.tools.Read(req)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, res)
	case protocol.Write:
		req, ok := decode[protocol.WriteRequest](w, env)
		if !ok {
			return
		}
		if err := s.tools.Write(req); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]int{"bytes": len(req.Content)})
	case protocol.Edit:
		req, ok := decode[protocol.EditRequest](w, env)
		if !ok {
			return
		}
		res, err := s.tools.Edit(req)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, res)
	case protocol.Glob:
		req, ok := decode[protocol.GlobRequest](w, env)
		if !ok {
			return
		}
		res, err := s.tools.Glob(req)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, res)
	case protocol.Grep:
		req, ok := decode[protocol.GrepRequest](w, env)
		if !ok {
			return
		}
		res, err := s.tools.Grep(ctx, req)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, res)
	case protocol.ProjectEnsure:
		req, ok := decode[protocol.ProjectRequest](w, env)
		if !ok {
			return
		}
		head, err := s.projects.Ensure(ctx, req.Project)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]string{"head": head})
	case protocol.ProjectBundle:
		req, ok := decode[protocol.ProjectRequest](w, env)
		if !ok {
			return
		}
		head, err := s.projects.Head(ctx, req.Project, req.Branch)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("X-Head", head)
		if head == req.Since {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var buf bytes.Buffer
		if err := s.projects.Bundle(ctx, req.Project, req.Branch, req.Since, &buf); err != nil {
			if errors.Is(err, ErrNothingNew) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-bundle")
		_, _ = io.Copy(w, &buf)
	default:
		http.Error(w, "unknown command kind "+env.Kind+"; update the sandbox", http.StatusBadRequest)
	}
}

// commandWithData takes a signed command in the X-Command header and the
// data it is about as the body; the signed payload names the body's hash.
func (s *Sandbox) commandWithData(w http.ResponseWriter, r *http.Request) {
	raw, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Command"))
	var sig protocol.Signed
	if err != nil || json.Unmarshal(raw, &sig) != nil {
		http.Error(w, "bad command", http.StatusBadRequest)
		return
	}
	env, ok := s.verify(w, &sig)
	if !ok {
		return
	}
	if env.Kind != protocol.ProjectSeed {
		http.Error(w, "this command carries no data", http.StatusBadRequest)
		return
	}
	req, ok := decode[protocol.ProjectRequest](w, env)
	if !ok {
		return
	}
	if free := diskFree(s.Cfg.DataDir); float64(req.Size)/(1<<30) > free {
		http.Error(w, "the bundle does not fit on the sandbox's disk", http.StatusInsufficientStorage)
		return
	}
	f, err := os.CreateTemp(s.Cfg.DataDir, "seed-*.bundle")
	if err != nil {
		fail(w, err)
		return
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, req.Size+1))
	f.Close()
	if err != nil || n != req.Size || hex.EncodeToString(h.Sum(nil)) != req.SHA256 {
		http.Error(w, "the bundle does not match the signed hash and size", http.StatusBadRequest)
		return
	}
	head, err := s.projects.Seed(r.Context(), req.Project, req.Branch, f.Name())
	if err != nil {
		fail(w, err)
		return
	}
	s.event("%s main at %.12s", req.Project, head)
	writeJSON(w, map[string]string{"main": head})
}

// servePreview passes a request on to an app listening in the sandbox,
// for Kapelo's preview addresses. Dev servers take a moment to listen, so
// it keeps trying for 20 seconds.
func (s *Sandbox) servePreview(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "not a port", http.StatusBadRequest)
		return
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var lastErr error
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		return nil, lastErr
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: addr})
			pr.Out.URL.Path = "/" + r.PathValue("rest")
			pr.Out.URL.RawPath = ""
			pr.Out.Host = pr.In.Header.Get("X-Forwarded-Host")
			pr.SetXForwarded()
		},
		Transport:     &http.Transport{DialContext: dial, ResponseHeaderTimeout: 5 * time.Minute},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, fmt.Sprintf("nothing answers on port %d in the sandbox: %v", port, err), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// Status is what the sandbox reports about itself.
func (s *Sandbox) Status() protocol.StatusResult {
	s.mu.Lock()
	st := protocol.StatusResult{SandboxID: s.identity().SandboxID, Name: s.Cfg.Name, Version: s.Version, Paused: s.paused}
	s.mu.Unlock()
	if s.projects != nil {
		st.Projects = s.projects.List()
	}
	if s.tools != nil {
		st.Processes, _ = s.tools.Process(protocol.ProcessRequest{Action: "list"})
	}
	if st.Processes == nil {
		st.Processes = []protocol.ProcessInfo{}
	}
	st.DiskFreeGB = diskFree(s.Cfg.DataDir)
	if s.db != nil {
		st.Database = s.db.URL
	}
	return st
}

// Pause is the owner's switch: background commands stop and no command
// runs until Resume.
func (s *Sandbox) Pause() int {
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
	n := 0
	if s.tools != nil {
		n = s.tools.StopAll()
	}
	s.event("paused by the owner; %d background commands stopped", n)
	return n
}

func (s *Sandbox) Resume() {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
	s.event("resumed by the owner")
}

func SocketPath(dataDir string) string { return filepath.Join(dataDir, fileSocket) }

// serveControl answers kapelo-sandbox status, pause and resume on a Unix
// socket in the data folder.
func (s *Sandbox) serveControl() (io.Closer, error) {
	path := SocketPath(s.Cfg.DataDir)
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		online, since, lastErr := s.session != nil, s.online, s.lastErr
		events := append([]string(nil), s.events...)
		s.mu.Unlock()
		writeJSON(w, map[string]any{"status": s.Status(), "online": online, "since": since, "last_error": lastErr, "events": events})
	})
	mux.HandleFunc("POST /pause", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]int{"stopped": s.Pause()})
	})
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, r *http.Request) {
		s.Resume()
		writeJSON(w, map[string]bool{"paused": false})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return srv, nil
}

// RenewBefore is how long before expiry the sandbox asks for a new
// certificate. Certificates last a year.
const RenewBefore = 30 * 24 * time.Hour

func (s *Sandbox) renewLoop(ctx context.Context) {
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		if err := s.maybeRenew(ctx); err != nil {
			s.event("certificate renewal failed, trying again later: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.linkUp:
		}
	}
}

func (s *Sandbox) maybeRenew(ctx context.Context) error {
	id := s.identity()
	leaf, err := x509.ParseCertificate(id.TLS.Certificates[0].Certificate[0])
	if err != nil {
		return err
	}
	before := s.RenewBefore
	if before == 0 {
		before = RenewBefore
	}
	if time.Until(leaf.NotAfter) > before {
		return nil
	}
	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()
	if sess == nil {
		return nil // offline; the next link tries again
	}
	keyPEM, csrPEM, err := newKeyAndCSR(s.Cfg.Name)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"csr": string(csrPEM)})
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodPost, "http://link/agent/renew", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := link.Client(sess).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Kapelo answered %s: %s", resp.Status, bytes.TrimSpace(rb))
	}
	var out struct {
		Cert string `json:"cert"`
	}
	if err := json.Unmarshal(rb, &out); err != nil || out.Cert == "" {
		return errors.New("no certificate in the answer")
	}
	pair := append([]byte(out.Cert), keyPEM...)
	cert, err := tls.X509KeyPair(pair, pair)
	if err != nil {
		return fmt.Errorf("the new certificate does not match the key: %w", err)
	}
	tmp := filepath.Join(s.Cfg.DataDir, filePair+".new")
	if err := os.WriteFile(tmp, pair, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.Cfg.DataDir, filePair)); err != nil {
		return err
	}
	newLeaf, _ := x509.ParseCertificate(cert.Certificate[0])
	s.idMu.Lock()
	cfg := s.ID.TLS.Clone()
	cfg.Certificates = []tls.Certificate{cert}
	s.ID = &Identity{SandboxID: s.ID.SandboxID, TLS: cfg, CommandKey: s.ID.CommandKey}
	s.idMu.Unlock()
	s.event("certificate renewed, valid until %s; used from the next connection", newLeaf.NotAfter.Format(time.DateOnly))
	return nil
}
