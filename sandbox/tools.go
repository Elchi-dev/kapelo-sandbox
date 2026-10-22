package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/Elchi-dev/kapelo-sandbox/gates"
	"github.com/Elchi-dev/kapelo-sandbox/protocol"
)

// Tools work inside project folders: <projects>/<name>. A path given to a
// tool is relative to its project and is refused when it, or a link on the
// way, leads out of it.
type Tools struct {
	Projects string
	Home     string
	// Env is added to every command: HOME, DATABASE_URL.
	Env func() []string

	mu    sync.Mutex
	procs map[string]*process
	next  int
}

const (
	defaultTimeout = 2 * time.Minute
	maxTimeout     = 10 * time.Minute
	outputHead     = 10_000
	outputTail     = 20_000
	readLines      = 2000
	readLineMax    = 2000
	maxFileRead    = 10 << 20
	maxFoundPaths  = 500
	maxMatches     = 200
	backgroundKeep = 256 << 10
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ErrRefused marks a request the sandbox will not carry out as asked; the
// message says why, for the agent to correct.
var ErrRefused = errors.New("refused")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

func (t *Tools) projectDir(project string) (string, error) {
	if !safeName.MatchString(project) {
		return "", refuse("%q is not a project name", project)
	}
	dir := filepath.Join(t.Projects, project)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", refuse("project %s has no folder in this sandbox yet", project)
	}
	return dir, nil
}

// resolve turns a path in a project into a path on disk, following links
// only as far as they stay inside the project.
func (t *Tools) resolve(project, rel string) (string, error) {
	base, err := t.projectDir(project)
	if err != nil {
		return "", err
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	if strings.ContainsRune(rel, 0) {
		return "", refuse("the path contains a NUL byte")
	}
	p := filepath.Join(realBase, filepath.Clean("/"+rel))
	// The longest part of the path that exists decides where it leads.
	existing := p
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	if real != realBase && !strings.HasPrefix(real, realBase+string(filepath.Separator)) {
		return "", refuse("%s leads out of the project", rel)
	}
	return filepath.Join(real, strings.TrimPrefix(p, existing)), nil
}

func (t *Tools) env(extra map[string]string) []string {
	env := os.Environ()
	if t.Env != nil {
		env = append(env, t.Env()...)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// clipOutput keeps the start and the end of long output, where commands
// put what matters.
func clipOutput(b []byte) (string, bool) {
	if len(b) <= outputHead+outputTail {
		return string(b), false
	}
	head, tail := b[:outputHead], b[len(b)-outputTail:]
	return fmt.Sprintf("%s\n\n... %d bytes left out ...\n\n%s", head, len(b)-outputHead-outputTail, tail), true
}

// Exec runs a command with bash in the project, or starts it in the
// background. The command and everything it starts run in their own
// process group, which is killed when the time is up.
func (t *Tools) Exec(ctx context.Context, r protocol.ExecRequest) (protocol.ExecResult, error) {
	var res protocol.ExecResult
	dir, err := t.resolve(r.Project, r.Dir)
	if err != nil {
		return res, err
	}
	if strings.TrimSpace(r.Command) == "" {
		return res, refuse("the command is empty")
	}
	if r.Background {
		id, err := t.startBackground(r, dir)
		res.ProcessID = id
		return res, err
	}
	timeout := time.Duration(r.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	cmd := exec.Command("bash", "-c", r.Command)
	cmd.Dir = dir
	cmd.Env = t.env(r.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return res, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(timeout):
		res.TimedOut = true
	case <-ctx.Done():
		res.TimedOut = true
	}
	if res.TimedOut {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	res.DurationMS = time.Since(start).Milliseconds()
	res.ExitCode = exitCode(err)
	if res.TimedOut {
		res.ExitCode = -1
	}
	res.Output, res.Truncated = clipOutput(out.Bytes())
	return res, nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	default:
		return -1
	}
}

// process is a command running in the background.
type process struct {
	info protocol.ProcessInfo
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  []byte
}

func (p *process) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out = append(p.out, b...)
	if len(p.out) > backgroundKeep {
		p.out = append([]byte(nil), p.out[len(p.out)-backgroundKeep:]...)
	}
	return len(b), nil
}

func (t *Tools) startBackground(r protocol.ExecRequest, dir string) (string, error) {
	cmd := exec.Command("bash", "-c", r.Command)
	cmd.Dir = dir
	cmd.Env = t.env(r.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	t.mu.Lock()
	if t.procs == nil {
		t.procs = map[string]*process{}
	}
	t.next++
	id := fmt.Sprintf("p%d", t.next)
	p := &process{cmd: cmd, info: protocol.ProcessInfo{ID: id, Project: r.Project, Command: r.Command, Started: time.Now().UTC(), Running: true}}
	t.procs[id] = p
	t.mu.Unlock()
	cmd.Stdout, cmd.Stderr = p, p
	if err := cmd.Start(); err != nil {
		t.mu.Lock()
		delete(t.procs, id)
		t.mu.Unlock()
		return "", err
	}
	go func() {
		code := exitCode(cmd.Wait())
		p.mu.Lock()
		p.info.Running = false
		p.info.ExitCode = &code
		p.mu.Unlock()
	}()
	return id, nil
}

// Process reads a background command's newest output, stops it, or lists
// them all.
func (t *Tools) Process(r protocol.ProcessRequest) ([]protocol.ProcessInfo, error) {
	t.mu.Lock()
	var list []*process
	if r.Action == "list" {
		for _, p := range t.procs {
			list = append(list, p)
		}
	} else if p := t.procs[r.ProcessID]; p != nil {
		list = []*process{p}
	}
	t.mu.Unlock()
	if r.Action != "list" && len(list) == 0 {
		return nil, refuse("no process %q", r.ProcessID)
	}
	var out []protocol.ProcessInfo
	for _, p := range list {
		switch r.Action {
		case "stop":
			if p.cmd.Process != nil {
				_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
				go func(p *process) {
					time.Sleep(5 * time.Second)
					_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
				}(p)
			}
		case "output", "list":
		default:
			return nil, refuse("action %q is not output, stop or list", r.Action)
		}
		p.mu.Lock()
		info := p.info
		if r.Action == "output" {
			info.Output, _ = clipOutput(p.out)
		}
		p.mu.Unlock()
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out, nil
}

// StopAll ends every background command, for the pause switch.
func (t *Tools) StopAll() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, p := range t.procs {
		p.mu.Lock()
		running := p.info.Running
		p.mu.Unlock()
		if running && p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			n++
		}
	}
	return n
}

// Read returns lines of a text file, numbered like cat -n.
func (t *Tools) Read(r protocol.ReadRequest) (protocol.ReadResult, error) {
	var res protocol.ReadResult
	p, err := t.resolve(r.Project, r.Path)
	if err != nil {
		return res, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return res, refuse("%s does not exist", r.Path)
	}
	if st.IsDir() {
		return res, refuse("%s is a folder; use glob to list it", r.Path)
	}
	if st.Size() > maxFileRead {
		return res, refuse("%s has %d MB; read it in parts with a command such as sed -n", r.Path, st.Size()>>20)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return res, err
	}
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 || !utf8.Valid(b[:min(len(b), 8000)]) {
		return res, refuse("%s is a binary file of %d bytes", r.Path, len(b))
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	res.TotalLines = len(lines)
	from := max(r.Offset, 1)
	limit := r.Limit
	if limit <= 0 || limit > readLines {
		limit = readLines
	}
	var sb strings.Builder
	for i := from - 1; i < len(lines) && i < from-1+limit; i++ {
		l := lines[i]
		if len(l) > readLineMax {
			l = l[:readLineMax] + " ..."
		}
		fmt.Fprintf(&sb, "%6d\t%s\n", i+1, l)
	}
	res.Content = sb.String()
	res.Truncated = from-1+limit < len(lines)
	return res, nil
}

// Write replaces a file, or makes it with its folders.
func (t *Tools) Write(r protocol.WriteRequest) error {
	p, err := t.resolve(r.Project, r.Path)
	if err != nil {
		return err
	}
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return refuse("%s is a folder", r.Path)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := p + ".kapelo-tmp"
	if err := os.WriteFile(tmp, []byte(r.Content), mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Edit replaces an exact piece of a file: once, where it occurs exactly
// once, or everywhere with All.
func (t *Tools) Edit(r protocol.EditRequest) (protocol.EditResult, error) {
	var res protocol.EditResult
	if r.Old == "" {
		return res, refuse("old is empty; use write to make a file")
	}
	if r.Old == r.New {
		return res, refuse("old and new are the same")
	}
	p, err := t.resolve(r.Project, r.Path)
	if err != nil {
		return res, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return res, refuse("%s does not exist", r.Path)
	}
	s := string(b)
	n := strings.Count(s, r.Old)
	switch {
	case n == 0:
		return res, refuse("old does not occur in %s; read the file and copy the text exactly", r.Path)
	case n > 1 && !r.All:
		return res, refuse("old occurs %d times in %s; give more context to make it unique, or set all", n, r.Path)
	}
	if r.All {
		s = strings.ReplaceAll(s, r.Old, r.New)
	} else {
		s = strings.Replace(s, r.Old, r.New, 1)
	}
	if err := t.Write(protocol.WriteRequest{Project: r.Project, Path: r.Path, Content: s}); err != nil {
		return res, err
	}
	res.Replaced = n
	if !r.All {
		res.Replaced = 1
	}
	return res, nil
}

// skipDir leaves out folders nobody wants searched, unless the pattern
// names them.
func skipDir(name, pattern string) bool {
	switch name {
	case ".git":
		return true
	case "node_modules", ".next", "dist", "build", ".cache", "vendor":
		return !strings.Contains(pattern, name)
	}
	return false
}

// Glob finds files by pattern, newest first.
func (t *Tools) Glob(r protocol.GlobRequest) (protocol.FilesResult, error) {
	var res protocol.FilesResult
	base, err := t.resolve(r.Project, "")
	if err != nil {
		return res, err
	}
	g, err := gates.CompileGlob(strings.TrimPrefix(r.Pattern, "/"))
	if err != nil {
		return res, refuse("%q is not a pattern: %v", r.Pattern, err)
	}
	type found struct {
		path string
		mod  time.Time
	}
	var all []found
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != base && skipDir(d.Name(), r.Pattern) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		if g.Match(rel) {
			info, _ := d.Info()
			if info != nil {
				all = append(all, found{rel, info.ModTime()})
			}
		}
		return nil
	})
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	for i, f := range all {
		if i == maxFoundPaths {
			res.Truncated = true
			break
		}
		res.Paths = append(res.Paths, f.path)
	}
	if res.Paths == nil {
		res.Paths = []string{}
	}
	return res, nil
}

// Grep searches file contents with a regular expression.
func (t *Tools) Grep(ctx context.Context, r protocol.GrepRequest) (protocol.GrepResult, error) {
	var res protocol.GrepResult
	base, err := t.resolve(r.Project, "")
	if err != nil {
		return res, err
	}
	expr := r.Pattern
	if r.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return res, refuse("%q is not a regular expression: %v", r.Pattern, err)
	}
	var g *gates.Glob
	if r.Glob != "" {
		gg, err := gates.CompileGlob(r.Glob)
		if err != nil {
			return res, refuse("%q is not a pattern: %v", r.Glob, err)
		}
		g = &gg
	}
	max := r.Max
	if max <= 0 || max > maxMatches {
		max = maxMatches
	}
	res.Matches = []protocol.Match{}
	stop := errors.New("enough")
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		if d.IsDir() {
			if p != base && skipDir(d.Name(), r.Glob) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		if g != nil && !g.Match(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for n := 1; sc.Scan(); n++ {
			line := sc.Bytes()
			if n == 1 && bytes.IndexByte(line, 0) >= 0 {
				return nil // binary
			}
			if re.Match(line) {
				text := string(line)
				if len(text) > 300 {
					text = text[:300] + " ..."
				}
				res.Matches = append(res.Matches, protocol.Match{Path: rel, Line: n, Text: text})
				if len(res.Matches) >= max {
					res.Truncated = true
					return stop
				}
			}
		}
		return nil
	})
	return res, nil
}
