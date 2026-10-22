// Package gates runs the deterministic checks a task must pass. They are
// tools, not models, and cost no tokens. Three of them are built in and run
// before the stack's own: scope (only allowed files changed), protected
// (implementers do not touch tests) and test budget (test writers stay
// within the milestone's number of tests).
package gates

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Elchi-dev/kapelo-sandbox/gitrepo"
	"github.com/Elchi-dev/kapelo-sandbox/stacks"
)

type Command struct {
	Name           string `json:"name"`
	Cmd            string `json:"cmd"`
	TimeoutMinutes int    `json:"timeout_minutes,omitempty"`
}

// Stack is a starter stack: setup script, rules for workers, gates.
type Stack struct {
	Name          string               `json:"name"`
	Setup         string               `json:"setup"`
	Start         string               `json:"start"` // run before every run, for services like the database
	Rules         string               `json:"rules"`
	TestGlobs     []string             `json:"test_globs"`
	AlwaysAllowed []string             `json:"always_allowed"`
	Gates         map[string][]Command `json:"gates"` // by kind: impl, test
	Final         []Command            `json:"final"`
	LineExts      []string             `json:"line_exts"`
	// Prepare runs in the worker container before the harness, such as
	// installing dependencies the clone does not carry.
	Prepare []Command `json:"prepare,omitempty"`
	// Database is "postgres" when the gates need one; the worker image
	// starts it inside the container.
	Database string `json:"database,omitempty"`

	Dir       string `json:"-"`
	RulesText string `json:"-"`
	extracted string // the temporary folder of a shipped stack
}

// Close removes the temporary copy of a shipped stack.
func (s *Stack) Close() {
	if s.extracted != "" {
		os.RemoveAll(s.extracted)
	}
}

func LoadStack(dir string) (*Stack, error) {
	b, err := os.ReadFile(filepath.Join(dir, "stack.json"))
	if err != nil {
		return nil, fmt.Errorf("stack: %w", err)
	}
	var s Stack
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("stack: parse stack.json: %w", err)
	}
	s.Dir = dir
	if s.Rules != "" {
		r, err := os.ReadFile(filepath.Join(dir, s.Rules))
		if err != nil {
			return nil, fmt.Errorf("stack: %w", err)
		}
		s.RulesText = string(r)
	}
	if len(s.Gates["impl"]) == 0 || len(s.Gates["test"]) == 0 {
		return nil, fmt.Errorf("stack: %s needs gates for impl and test", dir)
	}
	for _, p := range append(append([]string{}, s.TestGlobs...), s.AlwaysAllowed...) {
		if _, err := CompileGlob(p); err != nil {
			return nil, fmt.Errorf("stack: bad glob %q: %w", p, err)
		}
	}
	return &s, nil
}

// Kind maps a task type to its gate set.
func Kind(taskType string) string {
	if taskType == "test" {
		return "test"
	}
	return "impl"
}

type Result struct {
	Name   string
	Passed bool
	Output string // tail of the output, for the next attempt
}

type Input struct {
	Repo         gitrepo.Repo
	TaskType     string
	AllowedFiles []string
	TestBudget   int
}

// Run checks the task branch. It stops at the first failure: running tests
// on code that does not build tells nobody anything.
func (s *Stack) Run(ctx context.Context, in Input) ([]Result, error) {
	changed, err := in.Repo.Changed()
	if err != nil {
		return nil, err
	}
	var results []Result
	add := func(r Result) bool {
		results = append(results, r)
		return r.Passed
	}

	if Kind(in.TaskType) == "impl" {
		// Protected first: touched tests are put back, so the scope check
		// below judges only what remains.
		r := s.protected(changed)
		if !r.Passed {
			var touched []string
			for _, f := range changed {
				if MatchAny(s.TestGlobs, f) {
					touched = append(touched, f)
				}
			}
			if err := in.Repo.Restore(touched, "Restore protected test files"); err != nil {
				return nil, err
			}
		}
		if !add(r) {
			return results, nil
		}
	}
	if !add(s.scope(changed, in.AllowedFiles)) {
		return results, nil
	}
	if Kind(in.TaskType) == "test" {
		if !add(s.testBudget(in.Repo, changed, in.TestBudget)) {
			return results, nil
		}
	}
	for _, c := range s.Gates[Kind(in.TaskType)] {
		if !add(RunCommand(ctx, in.Repo.Dir, c)) {
			break
		}
	}
	return results, nil
}

func (s *Stack) scope(changed, allowed []string) Result {
	patterns := append(append([]string{}, allowed...), s.AlwaysAllowed...)
	var out []string
	for _, f := range changed {
		if !MatchAny(patterns, f) {
			out = append(out, f)
		}
	}
	if len(out) > 0 {
		return Result{Name: "scope", Output: "Changed files outside the allowed list: " +
			strings.Join(out, ", ") + ". Allowed: " + strings.Join(patterns, ", ")}
	}
	return Result{Name: "scope", Passed: true}
}

func (s *Stack) protected(changed []string) Result {
	var out []string
	for _, f := range changed {
		if MatchAny(s.TestGlobs, f) {
			out = append(out, f)
		}
	}
	if len(out) > 0 {
		return Result{Name: "protected", Output: "Test files are read only for implementation tasks and were restored: " +
			strings.Join(out, ", ") + ". Change the implementation so the tests pass, not the tests."}
	}
	return Result{Name: "protected", Passed: true}
}

var testCall = regexp.MustCompile(`\b(it|test)(\.each\([^)]*\))?\s*\(`)

func (s *Stack) testBudget(r gitrepo.Repo, changed []string, budget int) Result {
	if budget <= 0 {
		return Result{Name: "test-budget", Passed: true}
	}
	n := 0
	for _, f := range changed {
		if !MatchAny(s.TestGlobs, f) {
			continue
		}
		lines, err := r.AddedLines(f)
		if err != nil {
			return Result{Name: "test-budget", Output: err.Error()}
		}
		for _, l := range lines {
			n += len(testCall.FindAllString(l, -1))
		}
	}
	if n > budget {
		return Result{Name: "test-budget", Output: fmt.Sprintf(
			"%d tests added, the budget for this milestone is %d. Test only the acceptance criteria, at the API and user flow boundary, and merge cases that check the same behaviour.", n, budget)}
	}
	return Result{Name: "test-budget", Passed: true}
}

// RunCommand runs one gate command with bash in dir.
func RunCommand(ctx context.Context, dir string, c Command) Result {
	timeout := time.Duration(c.TimeoutMinutes) * time.Minute
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", c.Cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CI=1", "NEXT_TELEMETRY_DISABLED=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	res := Result{Name: c.Name, Passed: err == nil, Output: Tail(out.String(), 4000)}
	if ctx.Err() == context.DeadlineExceeded {
		res.Passed = false
		res.Output = fmt.Sprintf("timed out after %s\n%s", timeout, res.Output)
	}
	return res
}

// Tail keeps the last n bytes, where errors usually are.
func Tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}

// Map turns results into the report form.
func Map(rs []Result) map[string]bool {
	m := map[string]bool{}
	for _, r := range rs {
		m[r.Name] = r.Passed
	}
	return m
}

// Failed returns the failing result, if any.
func Failed(rs []Result) *Result {
	for i := range rs {
		if !rs[i].Passed {
			return &rs[i]
		}
	}
	return nil
}

// Find loads a stack by folder or by name. A name is looked up in each
// folder of dirs first, then among the stacks that ship with the worker,
// which are written to a temporary folder so their scripts can run; Close
// removes it.
func Find(nameOrPath string, dirs ...string) (*Stack, error) {
	if st, err := os.Stat(filepath.Join(nameOrPath, "stack.json")); err == nil && !st.IsDir() {
		return LoadStack(nameOrPath)
	}
	if strings.ContainsAny(nameOrPath, `/\`) {
		return nil, fmt.Errorf("stack: %s has no stack.json", nameOrPath)
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, nameOrPath, "stack.json")); err == nil {
			return LoadStack(filepath.Join(d, nameOrPath))
		}
	}
	tmp, err := os.MkdirTemp("", "kapelo-stack-")
	if err != nil {
		return nil, err
	}
	dir, err := stacks.Extract(nameOrPath, tmp)
	if err == nil {
		var st *Stack
		if st, err = LoadStack(dir); err == nil {
			st.extracted = tmp
			return st, nil
		}
	}
	os.RemoveAll(tmp)
	return nil, fmt.Errorf("stack: %w", err)
}
