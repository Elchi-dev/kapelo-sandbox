// Package worker is one attempt at a task, start to finish: prepare the
// clone, run the harness with the task card, commit, run the stack's
// checks, and say how it went.
package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Elchi-dev/kapelo-sandbox/gates"
	"github.com/Elchi-dev/kapelo-sandbox/gitrepo"
	"github.com/Elchi-dev/kapelo-sandbox/harness"
	"github.com/Elchi-dev/kapelo-sandbox/task"
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
