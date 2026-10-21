// Package gitrepo is the git work of a task: one branch per task, made
// from a base branch, committed after every attempt and compared with the
// base by the gates.
package gitrepo

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

type Repo struct {
	Dir string
	// Base is the branch tasks start from and are compared with; main
	// when empty.
	Base string
}

func (r Repo) base() string {
	if r.Base == "" {
		return "main"
	}
	return r.Base
}

func (r Repo) git(args ...string) (string, error) {
	full := append([]string{
		"-c", "user.name=Kapelo Worker", "-c", "user.email=worker@kapelo.invalid",
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = r.Dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Init makes the directory a repository with everything committed on the
// base branch.
func (r Repo) Init(message string) error {
	if _, err := r.git("rev-parse", "--git-dir"); err != nil {
		if _, err := r.git("init", "-q", "-b", r.base()); err != nil {
			return err
		}
	}
	// Scaffolders may have started on another branch name.
	if out, _ := r.git("rev-parse", "--verify", "-q", "HEAD"); out != "" {
		if _, err := r.git("branch", "-M", r.base()); err != nil {
			return err
		}
	} else if _, err := r.git("symbolic-ref", "HEAD", "refs/heads/"+r.base()); err != nil {
		return err
	}
	return r.Commit(message)
}

func Branch(taskID string) string { return "task/" + taskID }

// Current names the checked out branch.
func (r Repo) Current() (string, error) { return r.git("rev-parse", "--abbrev-ref", "HEAD") }

// EnsureBase makes a local base branch when a clone only has the remote
// one, as a clone of a task branch does.
func (r Repo) EnsureBase() error {
	if _, err := r.git("rev-parse", "--verify", "-q", r.base()); err == nil {
		return nil
	}
	if _, err := r.git("branch", r.base(), "origin/"+r.base()); err != nil {
		return fmt.Errorf("there is no %s branch to compare with: %w", r.base(), err)
	}
	return nil
}

// StartTask checks out the task branch, fresh from the base when reset is
// set or the branch does not exist yet.
func (r Repo) StartTask(taskID string, reset bool) error {
	b := Branch(taskID)
	if _, err := r.git("checkout", "-q", r.base()); err != nil {
		return err
	}
	if _, err := r.git("rev-parse", "--verify", "-q", b); err == nil && !reset {
		_, err := r.git("checkout", "-q", b)
		return err
	}
	_, err := r.git("checkout", "-q", "-B", b, r.base())
	return err
}

// Commit commits every change on the current branch, if there is any.
func (r Repo) Commit(message string) error {
	if _, err := r.git("add", "-A"); err != nil {
		return err
	}
	if out, _ := r.git("status", "--porcelain"); out == "" {
		return nil
	}
	_, err := r.git("commit", "-q", "-m", message)
	return err
}

// Dirty reports uncommitted changes, ignored files aside.
func (r Repo) Dirty() (bool, error) {
	out, err := r.git("status", "--porcelain")
	return out != "", err
}

// Untracked lists untracked paths that are not ignored; a directory with
// nothing tracked in it counts as one path ending in a slash.
func (r Repo) Untracked() ([]string, error) {
	out, err := r.git("status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(l, "?? "); ok {
			paths = append(paths, strings.Trim(p, `"`))
		}
	}
	return paths, nil
}

// Changed lists files the task branch changed against the base.
func (r Repo) Changed() ([]string, error) {
	out, err := r.git("diff", "--name-only", r.base()+"...HEAD")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// AddedLines returns the added lines of one file against the base.
func (r Repo) AddedLines(file string) ([]string, error) {
	out, err := r.git("diff", "-U0", r.base()+"...HEAD", "--", file)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
			lines = append(lines, l[1:])
		}
	}
	return lines, nil
}

// Diff is the task diff against the base.
func (r Repo) Diff() (string, error) { return r.git("diff", r.base()+"...HEAD") }

func (r Repo) DiffStat() (string, error) { return r.git("diff", "--stat", r.base()+"...HEAD") }

// Restore puts files back to their state on the base and commits that.
func (r Repo) Restore(files []string, message string) error {
	for _, f := range files {
		if _, err := r.git("cat-file", "-e", r.base()+":"+f); err == nil {
			if _, err := r.git("checkout", r.base(), "--", f); err != nil {
				return err
			}
		} else if _, err := r.git("rm", "-q", "-f", "--", f); err != nil {
			return err
		}
	}
	return r.Commit(message)
}

// Merge merges the task branch into the base and leaves the base checked
// out.
func (r Repo) Merge(taskID, message string) error {
	if _, err := r.git("checkout", "-q", r.base()); err != nil {
		return err
	}
	_, err := r.git("merge", "-q", "--no-ff", "-m", message, Branch(taskID))
	return err
}

func (r Repo) CheckoutBase() error {
	_, err := r.git("checkout", "-q", "-f", r.base())
	return err
}

// Clean drops uncommitted changes and untracked files that are not ignored.
func (r Repo) Clean() error {
	if _, err := r.git("reset", "-q", "--hard"); err != nil {
		return err
	}
	_, err := r.git("clean", "-q", "-fd")
	return err
}

// Lines counts tracked lines of code by extension, for reports.
func (r Repo) Lines(exts []string) (int, error) {
	out, err := r.git("ls-files")
	if err != nil {
		return 0, err
	}
	total := 0
	for _, f := range strings.Split(out, "\n") {
		for _, e := range exts {
			if strings.HasSuffix(f, e) {
				c, err := r.git("show", "HEAD:"+f)
				if err == nil {
					total += strings.Count(c, "\n") + 1
				}
				break
			}
		}
	}
	return total, nil
}
