// Package protocol is what the control plane and a sandbox say to each
// other: the enrollment exchange, and the signed envelope every command
// travels in, with the payloads of the tools.
//
// A sandbox runs nothing that is not signed by the control plane's key,
// addressed to its own sandbox id, inside its validity window and seen
// for the first time. A stolen connection alone is not enough to make a
// sandbox run anything.
package protocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Kinds of command.
const (
	Status = "status"

	// Tools, the way an agent works in the sandbox.
	Exec    = "tool.exec"    // run a shell command, or start one in the background
	Process = "tool.process" // read a background command's output, or stop it
	Read    = "tool.read"    // read a file, with line numbers
	Write   = "tool.write"   // write a whole file
	Edit    = "tool.edit"    // replace an exact piece of a file
	Glob    = "tool.glob"    // find files by pattern
	Grep    = "tool.grep"    // search file contents

	// Projects and git. The sandbox holds no git credentials: the control
	// plane fetches and pushes, and hands bundles over the link.
	ProjectEnsure = "project.ensure" // make the project folder, a git repository on main
	ProjectBundle = "project.bundle" // answer with a git bundle of a branch
	ProjectSeed   = "project.seed"   // bring a bundle into main; sent to /cmd-data with the bundle as body
)

type Envelope struct {
	ID        string          `json:"id"`
	SandboxID string          `json:"sandbox_id"`
	Kind      string          `json:"kind"`
	IssuedAt  time.Time       `json:"issued_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Signed carries the exact bytes that were signed, so verification never
// depends on how JSON is encoded again.
type Signed struct {
	Envelope []byte `json:"envelope"`
	Sig      []byte `json:"sig"`
}

// MaxLifetime bounds how long a command may stay valid.
const MaxLifetime = 5 * time.Minute

func Sign(key ed25519.PrivateKey, sandboxID, kind string, payload any, ttl time.Duration) (*Signed, error) {
	if ttl <= 0 || ttl > MaxLifetime {
		ttl = MaxLifetime
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	now := time.Now().UTC()
	env := Envelope{ID: hex.EncodeToString(id), SandboxID: sandboxID, Kind: kind, IssuedAt: now, ExpiresAt: now.Add(ttl), Payload: raw}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return &Signed{Envelope: b, Sig: ed25519.Sign(key, b)}, nil
}

// Verifier checks commands for one sandbox and remembers the ids it has
// accepted until they expire.
type Verifier struct {
	Key       ed25519.PublicKey
	SandboxID string
	Skew      time.Duration
	Now       func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

var (
	ErrSignature = errors.New("command: bad signature")
	ErrSandbox   = errors.New("command: addressed to another sandbox")
	ErrExpired   = errors.New("command: outside its validity window")
	ErrReplay    = errors.New("command: already seen")
)

func (v *Verifier) Verify(s *Signed) (*Envelope, error) {
	if len(v.Key) != ed25519.PublicKeySize || !ed25519.Verify(v.Key, s.Envelope, s.Sig) {
		return nil, ErrSignature
	}
	var env Envelope
	if err := json.Unmarshal(s.Envelope, &env); err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	if env.SandboxID != v.SandboxID {
		return nil, ErrSandbox
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	skew := v.Skew
	if skew == 0 {
		skew = time.Minute
	}
	if now.Before(env.IssuedAt.Add(-skew)) || now.After(env.ExpiresAt.Add(skew)) ||
		env.ExpiresAt.Sub(env.IssuedAt) > MaxLifetime {
		return nil, ErrExpired
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen == nil {
		v.seen = map[string]time.Time{}
	}
	for id, exp := range v.seen {
		if now.After(exp.Add(skew)) {
			delete(v.seen, id)
		}
	}
	if _, dup := v.seen[env.ID]; dup || env.ID == "" {
		return nil, ErrReplay
	}
	v.seen[env.ID] = env.ExpiresAt
	return &env, nil
}

// EnrollRequest and EnrollResponse are POST /agent/enroll. The sandbox
// makes its key pair and sends only the certificate request.
type EnrollRequest struct {
	Token    string  `json:"token"`
	Name     string  `json:"name"`
	CSR      string  `json:"csr"`
	Version  string  `json:"version"`
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
	DiskGB   int     `json:"disk_gb"`
	// Runtime is the container runtime the sandbox finds itself in, such
	// as runsc for gVisor, when it can tell.
	Runtime string `json:"runtime"`
}

type EnrollResponse struct {
	SandboxID  string `json:"sandbox_id"`
	Cert       string `json:"cert"`
	CA         string `json:"ca"`
	CommandKey string `json:"command_key"` // PKIX PEM, ed25519
}

// Every tool names the project it works in; paths are relative to the
// project's folder and never leave it.

type ExecRequest struct {
	Project string `json:"project"`
	Command string `json:"command"`
	// Dir is a folder inside the project to run in; the project's root
	// when empty.
	Dir        string            `json:"dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`
	// Background starts the command and answers at once with its process
	// id, for dev servers and long builds.
	Background bool `json:"background,omitempty"`
}

type ExecResult struct {
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"` // stdout and stderr as they came
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	ProcessID  string `json:"process_id,omitempty"` // for background commands
}

type ProcessRequest struct {
	// Action is output, stop or list.
	Action    string `json:"action"`
	ProcessID string `json:"process_id,omitempty"`
}

type ProcessInfo struct {
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Command  string    `json:"command"`
	Started  time.Time `json:"started"`
	Running  bool      `json:"running"`
	ExitCode *int      `json:"exit_code,omitempty"`
	Output   string    `json:"output,omitempty"` // the newest output, for output
}

type ReadRequest struct {
	Project string `json:"project"`
	Path    string `json:"path"`
	// Offset is the first line, from 1; Limit the number of lines.
	Offset int `json:"offset,omitempty"`
	Limit  int `json:"limit,omitempty"`
}

type ReadResult struct {
	Content    string `json:"content"` // lines as "<number>\t<text>"
	TotalLines int    `json:"total_lines"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type WriteRequest struct {
	Project string `json:"project"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

type EditRequest struct {
	Project string `json:"project"`
	Path    string `json:"path"`
	Old     string `json:"old"`
	New     string `json:"new"`
	// All replaces every occurrence; otherwise Old must occur exactly once.
	All bool `json:"all,omitempty"`
}

type EditResult struct {
	Replaced int `json:"replaced"`
}

type GlobRequest struct {
	Project string `json:"project"`
	Pattern string `json:"pattern"`
}

type GrepRequest struct {
	Project string `json:"project"`
	Pattern string `json:"pattern"` // a regular expression
	// Glob narrows the files searched, such as **/*.ts.
	Glob       string `json:"glob,omitempty"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	Max        int    `json:"max,omitempty"`
}

type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type GrepResult struct {
	Matches   []Match `json:"matches"`
	Truncated bool    `json:"truncated,omitempty"`
}

type FilesResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated,omitempty"`
}

// ProjectRequest names a project, and for bundle and seed a branch and
// what the other side already has.
type ProjectRequest struct {
	Project string `json:"project"`
	Branch  string `json:"branch,omitempty"`
	// Since is a commit the receiver has; the bundle leaves it out.
	Since string `json:"since,omitempty"`
	// SHA256 and Size describe the body sent with project.seed; the
	// sandbox refuses a body that does not match what was signed.
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// Status is what a sandbox reports about itself.
type StatusResult struct {
	SandboxID  string        `json:"sandbox_id"`
	Name       string        `json:"name"`
	Version    string        `json:"version"`
	Paused     bool          `json:"paused"`
	Projects   []string      `json:"projects"`
	Processes  []ProcessInfo `json:"processes"`
	DiskFreeGB float64       `json:"disk_free_gb"`
	Database   string        `json:"database,omitempty"` // DATABASE_URL commands get, when PostgreSQL runs
}
