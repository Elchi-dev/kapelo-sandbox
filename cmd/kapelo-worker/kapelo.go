package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Elchi-dev/kapelo-worker/gates"
	"github.com/Elchi-dev/kapelo-worker/gitrepo"
	"github.com/Elchi-dev/kapelo-worker/harness"
	"github.com/Elchi-dev/kapelo-worker/provider"
	"github.com/Elchi-dev/kapelo-worker/task"
	"github.com/Elchi-dev/kapelo-worker/worker"
)

// kapelo runs one attempt the way Kapelo starts it in a sandbox: the brief
// in KAPELO_TASK, the clone at /work on its task branch, the model behind
// the sandbox's relay at KAPELO_LLM_BASE. Kapelo decides the model, the
// branch and whether to try again; the worker only reports.
//
// It exits 0 once it has written its report line; how the attempt went
// is in the report. Any other exit means the worker itself broke.
func kapelo(ctx context.Context, e env) int {
	var b task.Brief
	if err := json.Unmarshal([]byte(e.getenv("KAPELO_TASK")), &b); err != nil || b.Task == 0 {
		fmt.Fprint(e.errw, usage)
		e.logf("KAPELO_TASK is missing or not a task: %v", err)
		return 2
	}
	report := func(r task.Report) int {
		if len(r.Summary) > 600 {
			r.Summary = r.Summary[len(r.Summary)-600:]
		}
		fmt.Fprintf(e.out, "\n%s\n", r.Line())
		return 0
	}
	fail := func(blocker, failure string) int {
		return report(task.Report{Status: "failed", Model: b.Model, Attempt: b.Attempt, Summary: blocker,
			Blockers: []string{blocker}, Failure: failure})
	}
	base := e.getenv("KAPELO_LLM_BASE")
	if base == "" {
		return fail("KAPELO_LLM_BASE is not set; inside Kapelo the agent sets it", "")
	}
	stack, err := gates.Find(b.Stack, e.getenv("KAPELO_STACKS"))
	if err != nil {
		return fail("the stack "+b.Stack+" is not available", err.Error())
	}
	defer stack.Close()
	repo := gitrepo.Repo{Dir: getenv(e, "KAPELO_WORKDIR", "/work")}
	if err := repo.EnsureBase(); err != nil {
		return fail("the clone has no main branch", err.Error())
	}
	stopDB, err := database(ctx, e, stack)
	if err != nil {
		return fail("the database for the checks did not start", err.Error())
	}
	defer stopDB()
	h := harness.Harness{Command: getenv(e, "KAPELO_HARNESS", "opencode"), Timeout: minutes(e, "KAPELO_WORKER_TIMEOUT_MIN", 20),
		ContextSize: number(e, "KAPELO_CONTEXT_SIZE", 128000)}
	// The relay adds the real key; the harness needs some key to send.
	ep := harness.Endpoint{Kind: provider.KindOpenAI, BaseURL: base, APIKey: "relay"}
	return report(worker.Attempt(ctx, b, worker.Options{Repo: repo, Stack: stack, Endpoint: ep, Harness: h, Logf: e.logf}))
}

func getenv(e env, k, def string) string {
	if v := e.getenv(k); v != "" {
		return v
	}
	return def
}

func number(e env, k string, def int) int {
	if n, err := strconv.Atoi(e.getenv(k)); err == nil && n > 0 {
		return n
	}
	return def
}

func minutes(e env, k string, def int) time.Duration {
	return time.Duration(number(e, k, def)) * time.Minute
}
