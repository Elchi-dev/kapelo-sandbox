// Package harness runs one attempt at a task in the coding harness
// (OpenCode): a session in the task's clone, pointed at one model
// provider, with the task card as its only instruction.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Elchi-dev/kapelo-sandbox/provider"
)

type Card struct {
	ID                 string
	Title              string
	Type               string
	Goal               string
	AcceptanceCriteria []string
	AllowedFiles       []string
	TestGlobs          []string
	TestBudget         int
	StackRules         string
	Notes              []string
	Attempt            int
	PreviousFailure    string // gate name and output of the last attempt
}

// Prompt renders the task card.
func (c Card) Prompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working on exactly one task in an existing repository. Do only this task.\n\n")
	fmt.Fprintf(&b, "# Task %s: %s\n\n%s\n\n", c.ID, c.Title, c.Goal)
	if len(c.AcceptanceCriteria) > 0 {
		b.WriteString("## Acceptance criteria\n\n")
		for _, a := range c.AcceptanceCriteria {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "## Files you may change\n\n%s\n\nChanges to any other file fail the task.\n\n", strings.Join(c.AllowedFiles, "\n"))
	if c.Type == "test" {
		fmt.Fprintf(&b, "## Writing the tests\n\n"+
			"- Test only the acceptance criteria above, at most %d tests in total.\n"+
			"- Test at the boundaries, such as API handlers and user flows. No tests for configuration, getters or trivial code.\n"+
			"- Use the real database through DATABASE_URL when the project has one. Mock only external services.\n"+
			"- The implementation may not exist yet. Write the tests against the interface the goal describes; they are expected to fail until it is built.\n"+
			"- Do not write the implementation.\n\n", c.TestBudget)
	} else {
		fmt.Fprintf(&b, "## Tests\n\nTest files (%s) are read only for you. Make the existing tests pass by changing the implementation. Do not add tests.\n\n",
			strings.Join(c.TestGlobs, ", "))
	}
	if c.StackRules != "" {
		fmt.Fprintf(&b, "## Stack rules\n\n%s\n\n", strings.TrimSpace(c.StackRules))
	}
	if len(c.Notes) > 0 {
		b.WriteString("## Notes\n\n")
		for _, n := range c.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\n")
	}
	if c.PreviousFailure != "" {
		fmt.Fprintf(&b, "## The previous attempt failed\n\nThe work of the previous attempt is still on this branch. It failed this check:\n\n```\n%s\n```\n\nFix the cause.\n\n", c.PreviousFailure)
	}
	b.WriteString("## Finishing\n\nRun the relevant checks yourself before you stop. Do not commit; the worker commits for you. " +
		"End with at most three sentences: what you changed, and anything that blocks you.\n")
	return b.String()
}

type Harness struct {
	Command     string
	Timeout     time.Duration
	ContextSize int
}

// Endpoint is where the harness sends its model calls.
type Endpoint struct {
	// Kind is provider.KindOpenAI or provider.KindAnthropic.
	Kind    string
	BaseURL string
	APIKey  string
}

// Config is the OpenCode configuration for one attempt: a single provider
// with every model the task may use, no sharing, no web access, no update
// checks.
func Config(e Endpoint, model string, models []string, contextSize int) string {
	ms := map[string]any{}
	for _, m := range append([]string{model}, models...) {
		ms[m] = map[string]any{"name": m, "limit": map[string]int{"context": contextSize, "output": 32000}}
	}
	npm := "@ai-sdk/openai-compatible"
	if e.Kind == provider.KindAnthropic {
		npm = "@ai-sdk/anthropic"
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			"kapelo": map[string]any{
				"npm":     npm,
				"name":    "Kapelo",
				"options": map[string]any{"baseURL": e.BaseURL, "apiKey": e.APIKey},
				"models":  ms,
			},
		},
		"enabled_providers": []string{"kapelo"},
		"model":             "kapelo/" + model,
		"small_model":       "kapelo/" + model,
		"permission":        map[string]string{"edit": "allow", "bash": "allow", "webfetch": "deny"},
		"share":             "disabled",
		"autoupdate":        false,
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

type Result struct {
	Summary string
	Log     string
	Err     error
}

// Run starts the harness in dir and waits for it.
func (h Harness) Run(ctx context.Context, dir string, e Endpoint, model string, models []string, prompt string) Result {
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.Command, "run", "--pure", "--auto", "--format", "json", "-m", "kapelo/"+model, prompt)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"OPENCODE_CONFIG_CONTENT="+Config(e, model, models, h.ContextSize),
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("worker timed out after %s", h.Timeout)
	} else if err != nil {
		err = fmt.Errorf("worker: %w: %s", err, tail(stderr.String(), 500))
	}
	return Result{
		Summary: Summary(stdout.String()),
		Log:     stdout.String() + "\n--- stderr ---\n" + stderr.String(),
		Err:     err,
	}
}

// Summary takes the last text the model wrote from the JSON event stream,
// shortened to roughly 150 tokens.
func Summary(out string) string {
	last := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var v any
		if json.Unmarshal([]byte(line), &v) != nil {
			continue
		}
		if t := lastText(v); t != "" {
			last = t
		}
	}
	if last == "" {
		last = out
	}
	return clip(strings.TrimSpace(last), 600)
}

// lastText finds the last {"type":"text","text":...} object in v.
func lastText(v any) string {
	found := ""
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "text" {
			if s, ok := x["text"].(string); ok && strings.TrimSpace(s) != "" {
				found = s
			}
		}
		for _, k := range []string{"part", "properties", "data", "message", "content"} {
			if c, ok := x[k]; ok {
				if t := lastText(c); t != "" {
					found = t
				}
			}
		}
	case []any:
		for _, e := range x {
			if t := lastText(e); t != "" {
				found = t
			}
		}
	}
	return found
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
