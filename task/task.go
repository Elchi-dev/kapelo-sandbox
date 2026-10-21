// Package task is what a worker is given and what it gives back: the
// brief of one task and the report of one attempt at it. Both travel as
// JSON, so anything that schedules work can drive the worker.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Types of task. A test task writes tests for behaviour that may not exist
// yet; every other type changes the implementation and leaves tests alone.
var Types = []string{"impl", "impl_hard", "frontend", "test"}

// Brief is one task, as the worker sees it.
type Brief struct {
	Task               int      `json:"task"`
	Title              string   `json:"title"`
	Type               string   `json:"type"`
	Goal               string   `json:"goal"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
	AllowedFiles       []string `json:"allowed_files"`
	Notes              []string `json:"notes,omitempty"`
	Attempt            int      `json:"attempt"`
	ModelOverride      string   `json:"model_override,omitempty"`
	Model              string   `json:"model"`
	Models             []string `json:"models,omitempty"`
	Stack              string   `json:"stack"`
	TestBudget         int      `json:"test_budget,omitempty"`
	Decisions          []string `json:"decisions,omitempty"`
	// PreviousFailure is the failing check of the attempt before. Its work
	// is still on the branch when the worker resumes it.
	PreviousFailure  string   `json:"previous_failure,omitempty"`
	PreviousSummary  string   `json:"previous_summary,omitempty"`
	PreviousBlockers []string `json:"previous_blockers,omitempty"`
}

// Check reports what a brief lacks before any work starts.
func (b Brief) Check() error {
	var errs []error
	if strings.TrimSpace(b.Goal) == "" {
		errs = append(errs, errors.New("the task has no goal"))
	}
	if len(b.AllowedFiles) == 0 {
		errs = append(errs, errors.New("the task allows no files; name the files or patterns it may change"))
	}
	ok := false
	for _, t := range Types {
		ok = ok || b.Type == t
	}
	if !ok {
		errs = append(errs, fmt.Errorf("type %q is not one of %s", b.Type, strings.Join(Types, ", ")))
	}
	if b.Stack == "" {
		errs = append(errs, errors.New("the task names no stack"))
	}
	return errors.Join(errs...)
}

// ReportPrefix starts the last line a worker writes; the JSON after it is
// a Report.
const ReportPrefix = "KAPELO_REPORT "

// Report is how an attempt went. Summary and blockers are short, for
// whoever plans the work; Failure is the failing check's output, for the
// next attempt.
type Report struct {
	Status   string          `json:"status"` // done, failed, blocked
	Model    string          `json:"model,omitempty"`
	Attempt  int             `json:"attempt,omitempty"`
	Summary  string          `json:"summary"`
	Blockers []string        `json:"blockers,omitempty"`
	Gates    map[string]bool `json:"gates,omitempty"`
	Failure  string          `json:"failure,omitempty"`
}

// Line renders the report as the worker's last line of output.
func (r Report) Line() string {
	j, _ := json.Marshal(r)
	return ReportPrefix + string(j)
}

// ParseLine reads a report line; ok is false for any other line.
func ParseLine(line string) (r Report, ok bool) {
	j, found := strings.CutPrefix(strings.TrimSpace(line), ReportPrefix)
	if !found {
		return r, false
	}
	return r, json.Unmarshal([]byte(j), &r) == nil
}
