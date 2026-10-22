package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Elchi-dev/kapelo-sandbox/protocol"
)

func newTools(t *testing.T) *Tools {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "shop", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "shop", "src", "a.ts"), []byte("export const a = 1;\nexport const b = 2;\n"), 0o644)
	os.WriteFile(filepath.Join(root, "shop", "README.md"), []byte("shop\n"), 0o644)
	return &Tools{Projects: root, Home: t.TempDir(), Env: func() []string { return []string{"KAPELO_TEST=1"} }}
}

func refused(err error) bool { return errors.Is(err, ErrRefused) }

func TestExec(t *testing.T) {
	tl := newTools(t)
	ctx := context.Background()
	res, err := tl.Exec(ctx, protocol.ExecRequest{Project: "shop", Command: "pwd; echo $KAPELO_TEST; echo oops >&2; exit 3", Dir: "src"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || !strings.HasSuffix(strings.Split(res.Output, "\n")[0], "/shop/src") ||
		!strings.Contains(res.Output, "1\noops") {
		t.Errorf("exec: %+v", res)
	}
	// A command that outlives its time is killed with what it started.
	start := time.Now()
	res, err = tl.Exec(ctx, protocol.ExecRequest{Project: "shop", Command: "sleep 30 & sleep 30; echo never", TimeoutSec: 1})
	if err != nil || !res.TimedOut || res.ExitCode != -1 || time.Since(start) > 5*time.Second || strings.Contains(res.Output, "never") {
		t.Errorf("timeout: %+v %v after %v", res, err, time.Since(start))
	}
	// Long output keeps its start and its end.
	res, _ = tl.Exec(ctx, protocol.ExecRequest{Project: "shop", Command: "echo first; head -c 100000 /dev/zero | tr '\\0' x; echo; echo last"})
	if !res.Truncated || !strings.HasPrefix(res.Output, "first") || !strings.HasSuffix(strings.TrimSpace(res.Output), "last") || len(res.Output) > 31000 {
		t.Errorf("long output: truncated %v, %d bytes", res.Truncated, len(res.Output))
	}
	if _, err := tl.Exec(ctx, protocol.ExecRequest{Project: "other", Command: "true"}); !refused(err) {
		t.Errorf("a project without a folder: %v", err)
	}
	if _, err := tl.Exec(ctx, protocol.ExecRequest{Project: "../shop", Command: "true"}); !refused(err) {
		t.Errorf("a path as project name: %v", err)
	}
}

func TestBackground(t *testing.T) {
	tl := newTools(t)
	res, err := tl.Exec(context.Background(), protocol.ExecRequest{Project: "shop", Command: "echo started; sleep 30", Background: true})
	if err != nil || res.ProcessID == "" {
		t.Fatalf("background: %+v %v", res, err)
	}
	time.Sleep(300 * time.Millisecond)
	out, err := tl.Process(protocol.ProcessRequest{Action: "output", ProcessID: res.ProcessID})
	if err != nil || len(out) != 1 || !out[0].Running || !strings.Contains(out[0].Output, "started") {
		t.Fatalf("output: %+v %v", out, err)
	}
	if _, err := tl.Process(protocol.ProcessRequest{Action: "stop", ProcessID: res.ProcessID}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, _ := tl.Process(protocol.ProcessRequest{Action: "list"})
		if len(list) == 1 && !list[0].Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running after stop: %+v", list)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := tl.Process(protocol.ProcessRequest{Action: "output", ProcessID: "p99"}); !refused(err) {
		t.Errorf("unknown process: %v", err)
	}
}

func TestFiles(t *testing.T) {
	tl := newTools(t)
	r, err := tl.Read(protocol.ReadRequest{Project: "shop", Path: "src/a.ts"})
	if err != nil || r.TotalLines != 2 || r.Content != "     1\texport const a = 1;\n     2\texport const b = 2;\n" {
		t.Fatalf("read: %q %v", r.Content, err)
	}
	r, _ = tl.Read(protocol.ReadRequest{Project: "shop", Path: "src/a.ts", Offset: 2, Limit: 1})
	if r.Content != "     2\texport const b = 2;\n" {
		t.Errorf("read part: %q", r.Content)
	}
	if err := tl.Write(protocol.WriteRequest{Project: "shop", Path: "src/new/b.ts", Content: "x\n"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(tl.Projects, "shop", "src", "new", "b.ts")); string(b) != "x\n" {
		t.Errorf("write: %q", b)
	}
	if _, err := tl.Edit(protocol.EditRequest{Project: "shop", Path: "src/a.ts", Old: "export const", New: "const"}); !refused(err) || !strings.Contains(err.Error(), "2 times") {
		t.Errorf("ambiguous edit: %v", err)
	}
	if e, err := tl.Edit(protocol.EditRequest{Project: "shop", Path: "src/a.ts", Old: "a = 1", New: "a = 10"}); err != nil || e.Replaced != 1 {
		t.Errorf("edit: %+v %v", e, err)
	}
	if e, err := tl.Edit(protocol.EditRequest{Project: "shop", Path: "src/a.ts", Old: "export const", New: "const", All: true}); err != nil || e.Replaced != 2 {
		t.Errorf("edit all: %+v %v", e, err)
	}
	if b, _ := os.ReadFile(filepath.Join(tl.Projects, "shop", "src", "a.ts")); string(b) != "const a = 10;\nconst b = 2;\n" {
		t.Errorf("after edits: %q", b)
	}
	if _, err := tl.Edit(protocol.EditRequest{Project: "shop", Path: "src/a.ts", Old: "nothing like this", New: "x"}); !refused(err) {
		t.Errorf("missing text: %v", err)
	}
	os.WriteFile(filepath.Join(tl.Projects, "shop", "logo.png"), []byte{0x89, 'P', 'N', 'G', 0, 0, 1}, 0o644)
	if _, err := tl.Read(protocol.ReadRequest{Project: "shop", Path: "logo.png"}); !refused(err) || !strings.Contains(err.Error(), "binary") {
		t.Errorf("binary file: %v", err)
	}

	os.MkdirAll(filepath.Join(tl.Projects, "shop", "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(tl.Projects, "shop", "node_modules", "x", "i.ts"), []byte("export const a = 1;\n"), 0o644)
	g, err := tl.Glob(protocol.GlobRequest{Project: "shop", Pattern: "**/*.ts"})
	if err != nil || len(g.Paths) != 2 || g.Paths[0] != "src/a.ts" {
		t.Errorf("glob: %v %v", g.Paths, err)
	}
	m, err := tl.Grep(context.Background(), protocol.GrepRequest{Project: "shop", Pattern: "CONST B", IgnoreCase: true, Glob: "**/*.ts"})
	if err != nil || len(m.Matches) != 1 || m.Matches[0].Path != "src/a.ts" || m.Matches[0].Line != 2 {
		t.Errorf("grep: %+v %v", m, err)
	}
}

// Every way out of a project is closed: dot dot, absolute paths, and links
// that point elsewhere, for reading as well as writing.
func TestConfinement(t *testing.T) {
	tl := newTools(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s3cret"), 0o644)
	os.Symlink(outside, filepath.Join(tl.Projects, "shop", "elsewhere"))
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(tl.Projects, "shop", "secret-link"))
	os.MkdirAll(filepath.Join(tl.Projects, "neighbour"), 0o755)
	os.WriteFile(filepath.Join(tl.Projects, "neighbour", "data"), []byte("theirs"), 0o644)

	for _, p := range []string{"elsewhere/secret", "secret-link"} {
		if _, err := tl.Read(protocol.ReadRequest{Project: "shop", Path: p}); !refused(err) || !strings.Contains(err.Error(), "leads out") {
			t.Errorf("read %s: %v", p, err)
		}
	}
	for _, p := range []string{"../neighbour/data", "/etc/passwd"} {
		r, err := tl.Read(protocol.ReadRequest{Project: "shop", Path: p})
		if err == nil && (strings.Contains(r.Content, "theirs") || strings.Contains(r.Content, "root:")) {
			t.Errorf("read %s reached outside: %q", p, r.Content)
		}
	}
	if err := tl.Write(protocol.WriteRequest{Project: "shop", Path: "elsewhere/new", Content: "x"}); !refused(err) {
		t.Errorf("write through a link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Error("a file was written outside the project")
	}
	if err := tl.Write(protocol.WriteRequest{Project: "shop", Path: "../neighbour/data", Content: "mine"}); err != nil {
		t.Fatal(err) // stays inside: it becomes shop/neighbour/data
	}
	if b, _ := os.ReadFile(filepath.Join(tl.Projects, "neighbour", "data")); string(b) != "theirs" {
		t.Errorf("the neighbour project was changed: %q", b)
	}
	if _, err := tl.Exec(context.Background(), protocol.ExecRequest{Project: "shop", Command: "true", Dir: "elsewhere"}); !refused(err) {
		t.Errorf("exec in a linked folder: %v", err)
	}
}
