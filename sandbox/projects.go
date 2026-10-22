package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Projects are folders in the data volume, each a git repository whose
// main branch Kapelo keeps in step with the project's remote. Agents work
// in them as they would on their own machine: on main or on a branch,
// committing as they go.
type Projects struct {
	Root string
	Env  func() []string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// gitConfig is the sandbox's own git configuration, in its home folder.
const gitConfig = `[user]
	name = Kapelo
	email = agent@kapelo.invalid
[commit]
	gpgsign = false
[init]
	defaultBranch = main
[safe]
	directory = *
`

func (p *Projects) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if p.Env != nil {
		cmd.Env = append(cmd.Env, p.Env()...)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (p *Projects) lock(project string) func() {
	p.mu.Lock()
	if p.locks == nil {
		p.locks = map[string]*sync.Mutex{}
	}
	l := p.locks[project]
	if l == nil {
		l = &sync.Mutex{}
		p.locks[project] = l
	}
	p.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (p *Projects) dir(project string) (string, error) {
	if !safeName.MatchString(project) {
		return "", refuse("%q is not a project name", project)
	}
	return filepath.Join(p.Root, project), nil
}

// List names the project folders.
func (p *Projects) List() []string {
	entries, _ := os.ReadDir(p.Root)
	out := []string{}
	for _, e := range entries {
		if e.IsDir() && safeName.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// Ensure makes the project's folder a repository on main with a first
// commit, unless it is one already.
func (p *Projects) Ensure(ctx context.Context, project string) (string, error) {
	dir, err := p.dir(project)
	if err != nil {
		return "", err
	}
	defer p.lock(project)()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return p.git(ctx, dir, "rev-parse", "HEAD")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := p.git(ctx, dir, "init", "-q", "-b", "main"); err != nil {
		return "", err
	}
	if _, err := p.git(ctx, dir, "commit", "-q", "--allow-empty", "-m", "Start "+project); err != nil {
		return "", err
	}
	return p.git(ctx, dir, "rev-parse", "HEAD")
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

var ErrNothingNew = errors.New("nothing new since that commit")

var branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func validBranch(b string) bool {
	return b != "" && !strings.HasPrefix(b, "-") && !strings.Contains(b, "..") && branchRe.MatchString(b)
}

// Head is the commit a branch points at.
func (p *Projects) Head(ctx context.Context, project, branch string) (string, error) {
	dir, err := p.dir(project)
	if err != nil {
		return "", err
	}
	if !validBranch(branch) {
		return "", refuse("%q is not a branch name", branch)
	}
	defer p.lock(project)()
	out, err := p.git(ctx, dir, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return "", refuse("project %s has no branch %s", project, branch)
	}
	return out, nil
}

// Bundle writes a git bundle of branch to out, without the history up to
// since when the repository has that commit.
func (p *Projects) Bundle(ctx context.Context, project, branch, since string, out io.Writer) error {
	dir, err := p.dir(project)
	if err != nil {
		return err
	}
	if !validBranch(branch) {
		return refuse("%q is not a branch name", branch)
	}
	defer p.lock(project)()
	head, err := p.git(ctx, dir, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return refuse("project %s has no branch %s", project, branch)
	}
	args := []string{"bundle", "create", "-", branch}
	if since != "" && shaRe.MatchString(since) {
		if _, err := p.git(ctx, dir, "merge-base", "--is-ancestor", since, branch); err == nil {
			if head == since {
				return ErrNothingNew
			}
			args = append(args, "^"+since)
		}
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if p.Env != nil {
		cmd.Env = append(cmd.Env, p.Env()...)
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git bundle: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Seed brings branch from a bundle file into the project's main. A
// project without a folder is cloned from it; otherwise main moves
// forward only when it has not gone its own way, and work in the folder
// is never overwritten.
func (p *Projects) Seed(ctx context.Context, project, branch, bundle string) (string, error) {
	dir, err := p.dir(project)
	if err != nil {
		return "", err
	}
	if !validBranch(branch) {
		return "", refuse("%q is not a branch name", branch)
	}
	defer p.lock(project)()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(p.Root, 0o755); err != nil {
			return "", err
		}
		if _, err := p.git(ctx, p.Root, "clone", "-q", "--branch", branch, bundle, dir); err != nil {
			return "", err
		}
		if branch != "main" {
			if _, err := p.git(ctx, dir, "branch", "-q", "-M", "main"); err != nil {
				return "", err
			}
		}
		_, _ = p.git(ctx, dir, "remote", "remove", "origin")
		return p.git(ctx, dir, "rev-parse", "main")
	}
	if _, err := p.git(ctx, dir, "bundle", "verify", "-q", bundle); err != nil {
		return "", err
	}
	if _, err := p.git(ctx, dir, "fetch", "-q", bundle, "+"+branch+":refs/kapelo/remote"); err != nil {
		return "", err
	}
	current, _ := p.git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if current == "main" {
		if dirty, _ := p.git(ctx, dir, "status", "--porcelain", "--untracked-files=no"); dirty != "" {
			return "", refuse("main is checked out with uncommitted changes in %s; commit or stash them first", project)
		}
		if _, err := p.git(ctx, dir, "merge", "-q", "--ff-only", "refs/kapelo/remote"); err != nil {
			return "", refuse("main in the sandbox has work the remote does not; push it first")
		}
	} else if _, err := p.git(ctx, dir, "fetch", "-q", ".", "refs/kapelo/remote:refs/heads/main"); err != nil {
		return "", refuse("main in the sandbox has work the remote does not; push it first")
	}
	return p.git(ctx, dir, "rev-parse", "main")
}
