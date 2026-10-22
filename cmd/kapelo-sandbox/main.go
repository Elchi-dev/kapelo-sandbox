// kapelo-sandbox is the Kapelo sandbox: one container on your own
// hardware where Kapelo's agents run commands, edit files and test your
// projects. It also carries a standalone worker that gives one coding
// task to a model of your choice.
//
//	kapelo-sandbox                   start, enrolling first with KAPELO_TOKEN
//	kapelo-sandbox status            what the running sandbox is doing
//	kapelo-sandbox pause | resume    stop every command, or allow them again
//	kapelo-sandbox worker ...        the standalone worker
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Elchi-dev/kapelo-sandbox/sandbox"
	"github.com/Elchi-dev/kapelo-sandbox/stacks"
)

var version = "dev"

const usage = `kapelo-sandbox gives Kapelo's agents a place to work on your own
hardware: they run commands, edit files and test your projects in it,
over one outbound connection, and only with commands Kapelo signed.

  kapelo-sandbox [serve]     start; the first time it enrolls with the
                             token in KAPELO_TOKEN (from Kapelo, sandbox settings)
  kapelo-sandbox enroll -token TOKEN [-name NAME] [-server HOST:PORT]
  kapelo-sandbox status      online or not, projects, running commands, recent events
  kapelo-sandbox pause       stop every running command and refuse new ones
  kapelo-sandbox resume
  kapelo-sandbox worker init|run|stacks   one coding task, without Kapelo
  kapelo-sandbox version

Environment: KAPELO_DATA (default /data), KAPELO_TOKEN, KAPELO_NAME,
KAPELO_SERVER (default agent.kapelo.app:443), KAPELO_POSTGRES=off.
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
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	code := 0
	switch cmd {
	case "serve":
		err = serve(ctx, e, args)
	case "enroll":
		err = enroll(ctx, e, args)
	case "status":
		err = status(e)
	case "pause", "resume":
		err = control(e, cmd)
	case "worker":
		code, err = workerCmd(ctx, e, args)
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
		fmt.Fprintln(e.errw, "kapelo-sandbox:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func workerCmd(ctx context.Context, e env, args []string) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(e.errw, workerUsage)
		return 2, nil
	}
	switch args[0] {
	case "init":
		return 0, initCmd(e, args[1:])
	case "run":
		return runCmd(ctx, e, args[1:])
	case "stacks":
		for _, n := range stacks.Names() {
			fmt.Fprintln(e.out, n)
		}
		return 0, nil
	}
	fmt.Fprint(e.errw, workerUsage)
	return 2, nil
}

func dataDir(e env) string { return getenv(e, "KAPELO_DATA", "/data") }

func enrollConfig(e env, name, server string) *sandbox.Config {
	if name == "" {
		name = e.getenv("KAPELO_NAME")
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	if server == "" {
		server = getenv(e, "KAPELO_SERVER", sandbox.DefaultServer)
	}
	c := &sandbox.Config{Name: name, Server: server, DataDir: dataDir(e)}
	if strings.EqualFold(e.getenv("KAPELO_POSTGRES"), "off") {
		off := false
		c.Postgres = &off
	}
	return c
}

func enroll(ctx context.Context, e env, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(e.errw)
	token := fs.String("token", "", "enrollment token from Kapelo (or KAPELO_TOKEN)")
	name := fs.String("name", "", "name shown in Kapelo (default: KAPELO_NAME or the host name)")
	server := fs.String("server", "", "Kapelo's sandbox endpoint (default: KAPELO_SERVER or "+sandbox.DefaultServer+")")
	ca := fs.String("ca", "", "trust this CA file for enrollment (for a self run Kapelo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		*token = e.getenv("KAPELO_TOKEN")
	}
	if *token == "" {
		return errors.New("no token: create one in Kapelo under sandboxes, then -token or KAPELO_TOKEN")
	}
	if *ca == "" {
		*ca = e.getenv("KAPELO_CA_FILE")
	}
	c := enrollConfig(e, *name, *server)
	id, err := sandbox.Enroll(ctx, c, *token, *ca, version)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.out, "Enrolled as %s (%s). Restart the container, or run kapelo-sandbox, to connect.\n", id, c.Name)
	return nil
}

// serve enrolls on the first start when a token is given, then runs.
func serve(ctx context.Context, e env, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("serve takes no arguments; see kapelo-sandbox help")
	}
	dir := dataDir(e)
	if !sandbox.Enrolled(dir) {
		token := e.getenv("KAPELO_TOKEN")
		if token == "" {
			return errors.New("this sandbox is not enrolled: start it with KAPELO_TOKEN set to a token from Kapelo (sandbox settings)")
		}
		c := enrollConfig(e, "", "")
		id, err := sandbox.Enroll(ctx, c, token, e.getenv("KAPELO_CA_FILE"), version)
		if err != nil {
			return err
		}
		fmt.Fprintf(e.out, "Enrolled as %s (%s).\n", id, c.Name)
	}
	cfg, err := sandbox.LoadConfig(dir)
	if err != nil {
		return err
	}
	ident, err := cfg.LoadIdentity()
	if err != nil {
		return err
	}
	s := &sandbox.Sandbox{Cfg: cfg, ID: ident, Version: version, PostgresPort: e.getenv("KAPELO_PG_PORT")}
	return s.Run(ctx)
}

func socketClient(e env) *http.Client {
	path := sandbox.SocketPath(dataDir(e))
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
}

func status(e env) error {
	resp, err := socketClient(e).Get("http://sandbox/status")
	if err != nil {
		return fmt.Errorf("the sandbox is not running here: %w", err)
	}
	defer resp.Body.Close()
	var st struct {
		Status struct {
			SandboxID  string   `json:"sandbox_id"`
			Name       string   `json:"name"`
			Version    string   `json:"version"`
			Paused     bool     `json:"paused"`
			Projects   []string `json:"projects"`
			DiskFreeGB float64  `json:"disk_free_gb"`
			Database   string   `json:"database"`
			Processes  []struct {
				ID, Project, Command string
				Running              bool
			} `json:"processes"`
		} `json:"status"`
		Online    bool      `json:"online"`
		Since     time.Time `json:"since"`
		LastError string    `json:"last_error"`
		Events    []string  `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return err
	}
	s := st.Status
	conn := "offline"
	if st.Online {
		conn = "online since " + st.Since.Local().Format(time.DateTime)
	} else if st.LastError != "" {
		conn = "offline: " + st.LastError
	}
	fmt.Fprintf(e.out, "%s (%s), version %s, %s\n", s.Name, s.SandboxID, s.Version, conn)
	if s.Paused {
		fmt.Fprintln(e.out, "PAUSED: no command runs until kapelo-sandbox resume")
	}
	fmt.Fprintf(e.out, "projects: %s\n", strings.Join(s.Projects, ", "))
	fmt.Fprintf(e.out, "disk free: %.1f GB\n", s.DiskFreeGB)
	if s.Database != "" {
		fmt.Fprintf(e.out, "database: %s\n", s.Database)
	}
	for _, p := range s.Processes {
		if p.Running {
			fmt.Fprintf(e.out, "running %s in %s: %s\n", p.ID, p.Project, p.Command)
		}
	}
	for _, ev := range st.Events[max(0, len(st.Events)-10):] {
		fmt.Fprintln(e.out, ev)
	}
	return nil
}

func control(e env, action string) error {
	resp, err := socketClient(e).Post("http://sandbox/"+action, "application/json", nil)
	if err != nil {
		return fmt.Errorf("the sandbox is not running here: %w", err)
	}
	resp.Body.Close()
	if action == "pause" {
		fmt.Fprintln(e.out, "Paused: running commands stopped, new ones are refused until kapelo-sandbox resume.")
	} else {
		fmt.Fprintln(e.out, "Resumed.")
	}
	return nil
}

func (e env) logf(format string, a ...any) {
	fmt.Fprintf(e.errw, "kapelo-sandbox: "+format+"\n", a...)
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

const workerUsage = `kapelo-sandbox worker gives one coding task to a model and checks the
result: only the allowed files changed, tests untouched, the stack's
build, lint and tests pass.

  kapelo-sandbox worker init   [-provider NAME] [-base-url URL] [-key-env VAR] [-model M] [-escalate M]
  kapelo-sandbox worker run    -goal TEXT -files PATTERNS [-title T] [-type impl] [-stack next-drizzle]
                               [-criteria A;B] [-provider NAME] [-model M] [-attempts 3] [-dir .] [-base main]
  kapelo-sandbox worker run    -task task.json [...]
  kapelo-sandbox worker stacks
`
