package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Elchi-dev/kapelo-worker/task"
)

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type fixture struct {
	tmp, origin, stacks, harness string
	vars                         map[string]string
}

// newFixture makes a repository, a small stack whose check needs page.txt
// to say "built", and a fake harness: a shell script standing in for
// OpenCode that does what the test says.
func newFixture(t *testing.T, harness string) *fixture {
	t.Helper()
	f := &fixture{tmp: t.TempDir(), vars: map[string]string{}}
	f.origin = filepath.Join(f.tmp, "origin")
	os.MkdirAll(f.origin, 0o755)
	sh(t, f.origin, "git", "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(f.origin, "README.md"), []byte("shop\n"), 0o644)
	sh(t, f.origin, "git", "add", "-A")
	sh(t, f.origin, "git", "commit", "-q", "-m", "init")

	f.stacks = filepath.Join(f.tmp, "stacks")
	tiny := filepath.Join(f.stacks, "tiny")
	os.MkdirAll(tiny, 0o755)
	os.WriteFile(filepath.Join(tiny, "rules.md"), []byte("Keep it small.\n"), 0o644)
	os.WriteFile(filepath.Join(tiny, "stack.json"), []byte(`{"name":"tiny","rules":"rules.md","test_globs":["**/*.test.txt"],
		"prepare":[{"name":"install","cmd":"touch .prepared"}],
		"gates":{"impl":[{"name":"page","cmd":"grep -q built page.txt"}],"test":[{"name":"none","cmd":"true"}]}}`), 0o644)
	f.harness = filepath.Join(f.tmp, "opencode")
	os.WriteFile(f.harness, []byte("#!/bin/sh\n"+harness+"\n"), 0o755)
	f.vars["KAPELO_STACKS"] = f.stacks
	f.vars["KAPELO_HARNESS"] = f.harness
	f.vars["KAPELO_WORKER_CONFIG"] = filepath.Join(f.tmp, "config.json")
	return f
}

func (f *fixture) run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	e := env{getenv: func(k string) string { return f.vars[k] }, in: bufio.NewReader(strings.NewReader(stdin)), out: &out, errw: &errw}
	code := dispatch(context.Background(), e, args)
	return code, out.String(), errw.String()
}

func reports(t *testing.T, out string) []task.Report {
	t.Helper()
	var rs []task.Report
	for _, l := range strings.Split(out, "\n") {
		if r, ok := task.ParseLine(l); ok {
			rs = append(rs, r)
		}
	}
	return rs
}

// Inside Kapelo: the clone is on a task branch with only origin/main, the
// brief is in the environment, one attempt, a report line at the end.
func TestKapeloMode(t *testing.T) {
	f := newFixture(t, `case "$*" in *"Keep it small."*"plain text only"*"no page.txt"*) ;; *) echo "prompt lacks the brief: $*" >&2; exit 3 ;; esac
case "$OPENCODE_CONFIG_CONTENT" in *'"apiKey":"relay","baseURL":"http://relay/t/x/v1"'*) ;; *) echo "config: $OPENCODE_CONFIG_CONTENT" >&2; exit 4 ;; esac
echo built > page.txt
echo '{"type":"text","text":"Wrote page.txt."}'`)
	sh(t, f.origin, "git", "branch", "task/t1-abc")
	work := filepath.Join(f.tmp, "work")
	sh(t, f.tmp, "git", "clone", "-q", "--branch", "task/t1-abc", f.origin, work)
	b, _ := json.Marshal(task.Brief{Task: 1, Title: "page", Type: "impl", Goal: "write page.txt", AllowedFiles: []string{"page.txt"},
		Attempt: 2, Model: "m/std", Models: []string{"m/std"}, Stack: "tiny", Decisions: []string{"plain text only"},
		PreviousFailure: "check page:\nno page.txt"})
	f.vars["KAPELO_TASK"], f.vars["KAPELO_WORKDIR"], f.vars["KAPELO_LLM_BASE"] = string(b), work, "http://relay/t/x/v1"

	code, out, errw := f.run(t, "")
	rs := reports(t, out)
	if code != 0 || len(rs) != 1 || rs[0].Status != "done" || rs[0].Summary != "Wrote page.txt." || !rs[0].Gates["page"] {
		t.Fatalf("exit %d, reports %+v\n%s%s", code, rs, out, errw)
	}
	if msg := sh(t, work, "git", "log", "-1", "--format=%s"); msg != "T1 attempt 2" {
		t.Errorf("commit %q", msg)
	}
	if _, err := os.Stat(filepath.Join(work, ".prepared")); err != nil {
		t.Error("prepare did not run")
	}
	if files := sh(t, work, "git", "show", "--name-only", "--format=", "HEAD"); files != "page.txt" {
		t.Errorf("the commit holds %q; what prepare made must stay out", files)
	}

	delete(f.vars, "KAPELO_TASK")
	if code, _, _ := f.run(t, ""); code != 2 {
		t.Errorf("without a brief: exit %d", code)
	}
}

func setUp(t *testing.T, f *fixture) {
	t.Helper()
	f.vars["TEST_KEY"] = "sk-test"
	code, out, errw := f.run(t, "", "init", "-provider", "mine", "-base-url", "https://models.example/v1", "-kind", "openai-compatible",
		"-key-env", "TEST_KEY", "-model", "m/small", "-escalate", "m/big")
	if code != 0 || !strings.Contains(out, "Saved provider mine") {
		t.Fatalf("init: %d %s %s", code, out, errw)
	}
}

// Standalone: the ladder. The fake model fails until the third attempt,
// which gets the stronger model; the second resumes the first's work and
// sees its failure.
func TestRunLadder(t *testing.T) {
	f := newFixture(t, `case "$*" in
  *"-m kapelo/m/big"*) test ! -f page.txt && echo built > page.txt; echo '{"type":"text","text":"Fixed it."}' ;;
  *"-m kapelo/m/small"*"check page:"*) grep -q first page.txt || exit 5; echo second >> page.txt ;;
  *"-m kapelo/m/small"*) echo first > page.txt ;;
esac
case "$OPENCODE_CONFIG_CONTENT" in *'"apiKey":"sk-test","baseURL":"https://models.example/v1"'*) ;; *) exit 4 ;; esac`)
	setUp(t, f)
	repoDir := f.origin
	code, out, errw := f.run(t, "", "run", "-dir", repoDir, "-stack", "tiny", "-goal", "write page.txt", "-files", "page.txt", "-title", "Page")
	rs := reports(t, out)
	if len(rs) != 3 || rs[0].Status != "failed" || rs[1].Status != "failed" || rs[2].Status != "done" ||
		rs[0].Model != "m/small" || rs[2].Model != "m/big" {
		t.Fatalf("exit %d, reports %+v\n%s", code, rs, errw)
	}
	// The third attempt started fresh from main: page.txt was not there.
	if code != 0 || sh(t, repoDir, "git", "rev-parse", "--abbrev-ref", "HEAD") != "task/page" {
		t.Fatalf("exit %d, branch %s", code, sh(t, repoDir, "git", "rev-parse", "--abbrev-ref", "HEAD"))
	}
	if page := sh(t, repoDir, "cat", "page.txt"); page != "built" {
		t.Errorf("page.txt = %q", page)
	}

	// Done with -merge lands on main.
	sh(t, repoDir, "git", "checkout", "-q", "main")
	code, _, errw = f.run(t, "", "run", "-dir", repoDir, "-stack", "tiny", "-goal", "write page.txt", "-files", "page.txt",
		"-title", "Page again", "-model", "m/big", "-attempts", "1", "-merge")
	if code != 0 || sh(t, repoDir, "git", "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Fatalf("merge run: %d\n%s", code, errw)
	}
}

func TestRunRefuses(t *testing.T) {
	f := newFixture(t, `exit 0`)
	if code, _, errw := f.run(t, "", "run", "-dir", f.origin, "-goal", "x", "-files", "a"); code != 1 || !strings.Contains(errw, "init") {
		t.Errorf("without a provider: %d %s", code, errw)
	}
	setUp(t, f)
	if code, _, errw := f.run(t, "", "run", "-dir", f.origin, "-goal", "x"); code != 2 || !strings.Contains(errw, "allows no files") {
		t.Errorf("without files: %d %s", code, errw)
	}
	os.WriteFile(filepath.Join(f.origin, "README.md"), []byte("changed\n"), 0o644)
	if code, _, errw := f.run(t, "", "run", "-dir", f.origin, "-stack", "tiny", "-goal", "x", "-files", "a"); code != 1 || !strings.Contains(errw, "uncommitted") {
		t.Errorf("dirty repository: %d %s", code, errw)
	}
	delete(f.vars, "TEST_KEY")
	sh(t, f.origin, "git", "checkout", "-q", "--", ".")
	if code, _, errw := f.run(t, "", "run", "-dir", f.origin, "-stack", "tiny", "-goal", "x", "-files", "a"); code != 1 || !strings.Contains(errw, "TEST_KEY is not set") {
		t.Errorf("key missing: %d %s", code, errw)
	}
}

func TestInitAsks(t *testing.T) {
	f := newFixture(t, `exit 0`)
	answers := "ollama\nllama3.3\n\n"
	code, out, errw := f.run(t, answers, "init")
	if code != 0 {
		t.Fatalf("init: %d %s %s", code, out, errw)
	}
	raw, _ := os.ReadFile(f.vars["KAPELO_WORKER_CONFIG"])
	if !strings.Contains(string(raw), `"base_url": "http://localhost:11434/v1"`) || !strings.Contains(string(raw), `"model": "llama3.3"`) ||
		!strings.Contains(string(raw), `"default": "ollama"`) {
		t.Errorf("config:\n%s", raw)
	}
	if st, _ := os.Stat(f.vars["KAPELO_WORKER_CONFIG"]); st.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v", st.Mode().Perm())
	}
	// Plain http to another machine is refused.
	if code, _, errw := f.run(t, "", "init", "-provider", "lan", "-base-url", "http://192.168.1.5/v1", "-kind", "openai-compatible", "-key-env", "X", "-model", "m"); code != 1 || !strings.Contains(errw, "https") {
		t.Errorf("http elsewhere: %d %s", code, errw)
	}
}

func TestStacksShip(t *testing.T) {
	f := newFixture(t, `exit 0`)
	if code, out, _ := f.run(t, "", "stacks"); code != 0 || !strings.Contains(out, "next-drizzle") {
		t.Errorf("stacks: %d %q", code, out)
	}
}
