// kapelo-worker lets a cheap model do one well defined coding task in a
// git repository, and checks the result before anybody looks at it.
//
//	kapelo-worker init                     set up a model provider
//	kapelo-worker run -goal ... -files ... one task, up to three attempts
//	kapelo-worker stacks                   the stacks that ship with it
//	kapelo-worker                          one attempt from KAPELO_TASK (inside Kapelo)
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Elchi-dev/kapelo-worker/stacks"
)

var version = "dev"

const usage = `kapelo-worker lets a model do one coding task in a git repository and
checks the result: only the allowed files changed, tests untouched, the
stack's build, lint and tests pass.

  kapelo-worker init      [-provider NAME] [-base-url URL] [-key-env VAR] [-model M] [-escalate M]
  kapelo-worker run       -goal TEXT -files PATTERNS [-title T] [-type impl] [-stack next-drizzle]
                          [-criteria A;B] [-provider NAME] [-model M] [-attempts 3] [-dir .] [-base main]
  kapelo-worker run       -task task.json [...]
  kapelo-worker stacks
  kapelo-worker version

Without a command it runs one attempt of the task in KAPELO_TASK against
KAPELO_LLM_BASE, the way Kapelo starts it in a sandbox.
`

type env struct {
	getenv func(string) string
	in     *bufio.Reader
	out    io.Writer
	errw   io.Writer
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	e := env{getenv: os.Getenv, in: bufio.NewReader(os.Stdin), out: os.Stdout, errw: os.Stderr}
	os.Exit(dispatch(ctx, e, os.Args[1:]))
}

func dispatch(ctx context.Context, e env, args []string) int {
	if len(args) == 0 {
		return kapelo(ctx, e)
	}
	var err error
	code := 0
	switch args[0] {
	case "init":
		err = initCmd(e, args[1:])
	case "run":
		code, err = runCmd(ctx, e, args[1:])
	case "stacks":
		for _, n := range stacks.Names() {
			fmt.Fprintln(e.out, n)
		}
	case "version":
		fmt.Fprintln(e.out, version)
	case "help", "-h", "--help":
		fmt.Fprint(e.out, usage)
	default:
		fmt.Fprint(e.errw, usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	if err != nil {
		fmt.Fprintln(e.errw, "kapelo-worker:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func (e env) logf(format string, a ...any) {
	fmt.Fprintf(e.errw, "kapelo-worker: "+format+"\n", a...)
}

// splitList reads "a;b;c" or "a,b" lists from a flag.
func splitList(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
