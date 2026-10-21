package task

import "testing"

func TestReportLine(t *testing.T) {
	r := Report{Status: "done", Summary: "Wrote it.", Gates: map[string]bool{"test": true}}
	back, ok := ParseLine("  " + r.Line() + "\n")
	if !ok || back.Status != "done" || back.Summary != "Wrote it." || !back.Gates["test"] {
		t.Errorf("round trip: %+v %v", back, ok)
	}
	if _, ok := ParseLine("npm notice something"); ok {
		t.Error("an ordinary line was read as a report")
	}
}

func TestBriefCheck(t *testing.T) {
	ok := Brief{Goal: "g", AllowedFiles: []string{"a"}, Type: "impl", Stack: "s"}
	if err := ok.Check(); err != nil {
		t.Error(err)
	}
	bad := Brief{Type: "deploy"}
	if err := bad.Check(); err == nil {
		t.Error("an empty brief passed")
	}
}
