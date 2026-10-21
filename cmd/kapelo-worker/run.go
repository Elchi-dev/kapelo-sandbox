package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/Elchi-dev/kapelo-worker/gates"
	"github.com/Elchi-dev/kapelo-worker/gitrepo"
	"github.com/Elchi-dev/kapelo-worker/harness"
	"github.com/Elchi-dev/kapelo-worker/provider"
	"github.com/Elchi-dev/kapelo-worker/task"
	"github.com/Elchi-dev/kapelo-worker/worker"
)

// runCmd does one task in a repository with up to three attempts: the
// model on a fresh branch, the same model on its own work with the failing
// check, then the stronger model on a fresh branch. The task branch stays
// checked out for review; -merge merges it into the base when done.
func runCmd(ctx context.Context, e env, args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(e.errw)
	taskFile := fs.String("task", "", "task as JSON (the fields of a brief); flags below fill or override it")
	goal := fs.String("goal", "", "what the task must achieve")
	title := fs.String("title", "", "short name of the task")
	typ := fs.String("type", "", "impl, impl_hard, frontend or test (default impl)")
	files := fs.String("files", "", "files or patterns the task may change, comma separated (src/app/api/**,src/db/schema.ts)")
	criteria := fs.String("criteria", "", "acceptance criteria, separated by ;")
	notes := fs.String("notes", "", "notes for the model, separated by ;")
	stackName := fs.String("stack", "", "stack name or folder (default next-drizzle; kapelo-worker stacks lists them)")
	prov := fs.String("provider", "", "provider from the configuration (default: the configured default)")
	model := fs.String("model", "", "model for the first two attempts (default: the provider's)")
	escalate := fs.String("escalate", "", "model for the third attempt (default: the provider's escalate)")
	attempts := fs.Int("attempts", 3, "attempts, 1 to 3")
	dir := fs.String("dir", ".", "the git repository")
	base := fs.String("base", "", "branch the task starts from and is compared with (default: the current branch)")
	id := fs.String("id", "", "task id, part of the branch name task/ID (default: from the title)")
	resume := fs.Bool("resume", false, "start from the task branch's work instead of a fresh branch")
	merge := fs.Bool("merge", false, "merge the task branch into the base when the task is done")
	timeout := fs.Int("timeout", 20, "minutes per attempt for the model")
	reportFile := fs.String("report", "", "write the last report as JSON to this file")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}

	var b task.Brief
	if *taskFile != "" {
		raw, err := os.ReadFile(*taskFile)
		if err != nil {
			return 2, err
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			return 2, fmt.Errorf("%s: %w", *taskFile, err)
		}
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&b.Goal, *goal)
	set(&b.Title, *title)
	set(&b.Type, *typ)
	set(&b.Stack, *stackName)
	if *files != "" {
		b.AllowedFiles = splitList(*files, ",")
	}
	if *criteria != "" {
		b.AcceptanceCriteria = splitList(*criteria, ";")
	}
	if *notes != "" {
		b.Notes = append(b.Notes, splitList(*notes, ";")...)
	}
	if b.Type == "" {
		b.Type = "impl"
	}
	if b.Stack == "" {
		b.Stack = "next-drizzle"
	}
	if b.Title == "" {
		b.Title = firstWords(b.Goal, 8)
	}
	if b.Task == 0 {
		b.Task = 1
	}
	if err := b.Check(); err != nil {
		return 2, err
	}
	if *attempts < 1 || *attempts > 3 {
		return 2, errors.New("-attempts must be 1, 2 or 3")
	}

	cfg, err := provider.Load(provider.Path(e.getenv))
	if err != nil {
		return 1, err
	}
	pname, p, err := cfg.Get(*prov)
	if err != nil {
		return 1, err
	}
	set(&p.Model, *model)
	set(&p.Escalate, *escalate)
	if err := p.Check(); err != nil {
		return 1, fmt.Errorf("provider %s: %w", pname, err)
	}
	key, err := p.Key(e.getenv)
	if err != nil {
		return 1, fmt.Errorf("provider %s: %w", pname, err)
	}

	stack, err := gates.Find(b.Stack, e.getenv("KAPELO_STACKS"))
	if err != nil {
		return 1, err
	}
	defer stack.Close()
	repo := gitrepo.Repo{Dir: *dir, Base: *base}
	if dirty, err := repo.Dirty(); err != nil {
		return 1, fmt.Errorf("%s is not a git repository with a commit: %w", *dir, err)
	} else if dirty {
		return 1, errors.New("the repository has uncommitted changes; commit or stash them first")
	}
	if repo.Base == "" {
		cur, err := repo.Current()
		if err != nil {
			return 1, err
		}
		if strings.HasPrefix(cur, "task/") {
			return 1, fmt.Errorf("%s is a task branch; give -base", cur)
		}
		repo.Base = cur
	}
	taskID := *id
	if taskID == "" {
		taskID = slug(b.Title)
	}
	stopDB, err := database(ctx, e, stack)
	if err != nil {
		return 1, err
	}
	defer stopDB()

	h := harness.Harness{Command: getenv(e, "KAPELO_HARNESS", "opencode"), Timeout: minutesOf(*timeout), ContextSize: p.ContextSize}
	if h.ContextSize == 0 {
		h.ContextSize = 128000
	}
	ep := harness.Endpoint{Kind: p.Kind, BaseURL: p.BaseURL, APIKey: key}
	b.Models = uniq(p.Model, p.Escalate)
	e.logf("task %s on branch %s from %s, provider %s", taskID, gitrepo.Branch(taskID), repo.Base, pname)

	var rep task.Report
	for step := 1; step <= *attempts; step++ {
		fresh := step != 2 && !(step == 1 && *resume)
		b.Model = p.Model
		if step == 3 && p.Escalate != "" {
			b.Model = p.Escalate
		}
		b.Attempt = step
		if err := repo.StartTask(taskID, fresh); err != nil {
			return 1, err
		}
		rep = worker.Attempt(ctx, b, worker.Options{Repo: repo, Stack: stack, Endpoint: ep, Harness: h, Logf: e.logf})
		fmt.Fprintln(e.out, rep.Line())
		if rep.Status != "failed" || ctx.Err() != nil {
			break
		}
		b.PreviousFailure, b.PreviousSummary, b.PreviousBlockers = rep.Failure, rep.Summary, rep.Blockers
	}
	if *reportFile != "" {
		j, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*reportFile, append(j, '\n'), 0o644); err != nil {
			return 1, err
		}
	}
	if rep.Status != "done" {
		e.logf("not done after %d attempts; the work is on %s", rep.Attempt, gitrepo.Branch(taskID))
		return 1, nil
	}
	if *merge {
		if err := repo.Merge(taskID, fmt.Sprintf("%s: %s", taskID, b.Title)); err != nil {
			return 1, err
		}
		e.logf("done and merged into %s", repo.Base)
		return 0, nil
	}
	e.logf("done; the work is on %s", gitrepo.Branch(taskID))
	return 0, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

func firstWords(s string, n int) string {
	w := strings.Fields(s)
	if len(w) > n {
		w = w[:n]
	}
	return strings.Join(w, " ")
}

func uniq(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" && !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// database starts PostgreSQL for the checks when the stack needs one and
// DATABASE_URL does not name one already.
func database(ctx context.Context, e env, stack *gates.Stack) (func(), error) {
	if stack.Database != "postgres" || e.getenv("DATABASE_URL") != "" {
		return func() {}, nil
	}
	pg, err := worker.StartPostgres(ctx, getenv(e, "KAPELO_PG_PORT", "54329"))
	if err != nil {
		return nil, err
	}
	e.logf("postgres for the checks at %s", pg.URL)
	if err := os.Setenv("DATABASE_URL", pg.URL); err != nil {
		pg.Stop()
		return nil, err
	}
	return pg.Stop, nil
}
