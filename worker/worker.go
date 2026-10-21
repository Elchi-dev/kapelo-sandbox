// Package worker is one attempt at a task, start to finish: prepare the
// clone, run the harness with the task card, commit, run the stack's
// checks, and say how it went.
package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/Elchi-dev/kapelo-worker/gates"
	"github.com/Elchi-dev/kapelo-worker/gitrepo"
	"github.com/Elchi-dev/kapelo-worker/harness"
	"github.com/Elchi-dev/kapelo-worker/task"
)

type Options struct {
	Repo     gitrepo.Repo
	Stack    *gates.Stack
	Endpoint harness.Endpoint
	Harness  harness.Harness
	Logf     func(format string, a ...any)
}

func (o Options) logf(format string, a ...any) {
	if o.Logf != nil {
		o.Logf(format, a...)
	}
}

// Attempt runs one attempt on the branch that is checked out. It never
// fails as a function: whatever goes wrong is in the report.
func Attempt(ctx context.Context, b task.Brief, o Options) task.Report {
	rep := task.Report{Model: b.Model, Attempt: b.Attempt}
	failed := func(blocker, failure string) task.Report {
		rep.Status = "failed"
		if len(blocker) > 200 {
			blocker = blocker[:200] + "..."
		}
		rep.Blockers = append(rep.Blockers, blocker)
		rep.Failure = failure
		if rep.Summary == "" {
			rep.Summary = blocker
		}
		return rep
	}
	if err := b.Check(); err != nil {
		return failed("the brief is incomplete", err.Error())
	}
	if b.Model == "" {
		return failed("the brief names no model", "")
	}
	for _, c := range o.Stack.Prepare {
		o.logf("prepare: %s", c.Name)
		if res := gates.RunCommand(ctx, o.Repo.Dir, c); !res.Passed {
			return failed("prepare "+c.Name+" failed", "prepare "+c.Name+":\n"+res.Output)
		}
	}
	if err := excludeUntracked(o.Repo); err != nil {
		return failed("the prepared clone could not be read", err.Error())
	}

	card := harness.Card{ID: fmt.Sprintf("T%d", b.Task), Title: b.Title, Type: b.Type, Goal: b.Goal,
		AcceptanceCriteria: b.AcceptanceCriteria, AllowedFiles: b.AllowedFiles, TestGlobs: o.Stack.TestGlobs,
		TestBudget: b.TestBudget, StackRules: o.Stack.RulesText, Notes: b.Notes, Attempt: b.Attempt,
		PreviousFailure: b.PreviousFailure}
	for _, d := range b.Decisions {
		card.Notes = append(card.Notes, "Project decision: "+d)
	}
	o.logf("T%d attempt %d with %s", b.Task, b.Attempt, b.Model)
	res := o.Harness.Run(ctx, o.Repo.Dir, o.Endpoint, b.Model, b.Models, card.Prompt())
	if ctx.Err() != nil {
		return failed("the attempt was stopped", "")
	}
	rep.Summary = res.Summary
	if err := o.Repo.Commit(fmt.Sprintf("T%d attempt %d", b.Task, b.Attempt)); err != nil {
		return failed("the work could not be committed", err.Error())
	}
	if res.Err != nil {
		return failed(res.Err.Error(), res.Err.Error())
	}
	results, err := o.Stack.Run(ctx, gates.Input{Repo: o.Repo, TaskType: b.Type, AllowedFiles: b.AllowedFiles, TestBudget: b.TestBudget})
	if err != nil {
		return failed("the checks could not run", err.Error())
	}
	rep.Gates = gates.Map(results)
	for _, g := range results {
		o.logf("check %s: %v", g.Name, g.Passed)
	}
	if f := gates.Failed(results); f != nil {
		return failed("check "+f.Name+" failed", "check "+f.Name+":\n"+f.Output)
	}
	rep.Status = "done"
	return rep
}

// excludeUntracked keeps what prepare made (dependencies, caches) out of
// the task's commit, so the scope check judges only the harness's work.
func excludeUntracked(r gitrepo.Repo) error {
	paths, err := r.Untracked()
	if err != nil || len(paths) == 0 {
		return err
	}
	gitDir := filepath.Join(r.Dir, ".git")
	if st, err := os.Stat(gitDir); err != nil || !st.IsDir() {
		return errors.New("the clone's .git is not a folder")
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "info"), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(gitDir, "info", "exclude"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var b strings.Builder
	b.WriteString("# made by prepare\n")
	for _, p := range paths {
		b.WriteString("/" + p + "\n")
	}
	_, err = f.WriteString(b.String())
	return err
}

// Postgres starts a throwaway database for the checks: data in a new
// temporary folder, reachable on loopback only, user and password app.
// Stop removes it again.
type Postgres struct {
	URL  string
	root string // the temporary folder: data, log, socket, passwd
	env  []string
}

func StartPostgres(ctx context.Context, port string) (*Postgres, error) {
	if _, err := exec.LookPath("pg_ctl"); err != nil {
		return nil, errors.New("the checks need PostgreSQL: install it (pg_ctl on PATH) or set DATABASE_URL")
	}
	root, err := os.MkdirTemp("", "kapelo-pg-")
	if err != nil {
		return nil, err
	}
	data := filepath.Join(root, "data")
	p := &Postgres{root: root}
	env, err := passwdFor(root)
	if err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	run := func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), env...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, gates.Tail(out.String(), 1000))
		}
		return nil
	}
	p.env = env
	if err := run("initdb", "-D", data, "-U", "app", "--auth=trust", "-E", "UTF8", "--no-instructions"); err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	if err := run("pg_ctl", "-D", data, "-w", "-l", filepath.Join(root, "log"), "-o", "-h 127.0.0.1 -p "+port+" -k "+root+" -F", "start"); err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	if err := run("createdb", "-h", "127.0.0.1", "-p", port, "-U", "app", "app"); err != nil {
		p.Stop()
		return nil, err
	}
	p.URL = "postgres://app:app@127.0.0.1:" + port + "/app"
	return p, nil
}

func (p *Postgres) Stop() {
	cmd := exec.Command("pg_ctl", "-D", filepath.Join(p.root, "data"), "-m", "fast", "stop")
	cmd.Env = append(os.Environ(), p.env...)
	_ = cmd.Run()
	os.RemoveAll(p.root)
}

// nssWrapper is where Debian's libnss-wrapper lives; the worker image has
// it.
var nssWrapper = []string{"/usr/lib/x86_64-linux-gnu/libnss_wrapper.so", "/usr/lib/aarch64-linux-gnu/libnss_wrapper.so"}

// passwdFor lets PostgreSQL run as a user id without an /etc/passwd
// entry, as in a container started with --user to match the owner of a
// mounted repository: initdb refuses such a user. nss_wrapper hands the
// PostgreSQL programs a passwd file with that id in it.
func passwdFor(dir string) ([]string, error) {
	if _, err := user.Current(); err == nil {
		return nil, nil
	}
	lib := ""
	for _, l := range nssWrapper {
		if _, err := os.Stat(l); err == nil {
			lib = l
		}
	}
	if lib == "" {
		return nil, fmt.Errorf("user id %d has no passwd entry, which PostgreSQL needs; run as a known user or set DATABASE_URL", os.Getuid())
	}
	uid, gid := os.Getuid(), os.Getgid()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte(fmt.Sprintf("worker:x:%d:%d:worker:%s:/bin/sh\n", uid, gid, os.TempDir())), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(group, []byte(fmt.Sprintf("worker:x:%d:\n", gid)), 0o644); err != nil {
		return nil, err
	}
	return []string{"LD_PRELOAD=" + lib, "NSS_WRAPPER_PASSWD=" + passwd, "NSS_WRAPPER_GROUP=" + group}, nil
}
