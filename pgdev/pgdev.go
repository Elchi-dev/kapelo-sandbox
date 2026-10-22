// Package pgdev runs a PostgreSQL for development and checks: on
// loopback only, user and password app, database app, trusting every local
// connection. It needs PostgreSQL's programs on PATH.
package pgdev

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

type Server struct {
	URL  string
	root string // data, log, socket and passwd live here
	env  []string
	temp bool
}

// Available reports whether PostgreSQL's programs are on PATH.
func Available() bool {
	_, err := exec.LookPath("pg_ctl")
	return err == nil
}

// Temporary starts a database in a new temporary folder; Stop removes it.
func Temporary(ctx context.Context, port string) (*Server, error) {
	root, err := os.MkdirTemp("", "kapelo-pg-")
	if err != nil {
		return nil, err
	}
	s, err := start(ctx, root, port)
	if err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	s.temp = true
	return s, nil
}

// Persistent starts the database kept in root, making it the first time.
// Stop shuts it down and keeps the data.
func Persistent(ctx context.Context, root, port string) (*Server, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return start(ctx, root, port)
}

func start(ctx context.Context, root, port string) (*Server, error) {
	if !Available() {
		return nil, errors.New("PostgreSQL is not installed here (pg_ctl is not on PATH)")
	}
	env, err := passwdFor(root)
	if err != nil {
		return nil, err
	}
	s := &Server{root: root, env: env}
	data := filepath.Join(root, "data")
	fresh := false
	if _, err := os.Stat(filepath.Join(data, "PG_VERSION")); err != nil {
		if err := s.run(ctx, "initdb", "-D", data, "-U", "app", "--auth=trust", "-E", "UTF8", "--no-instructions"); err != nil {
			return nil, err
		}
		fresh = true
	}
	// A container that was killed leaves a stale pid file behind.
	_ = os.Remove(filepath.Join(data, "postmaster.pid"))
	if err := s.run(ctx, "pg_ctl", "-D", data, "-w", "-l", filepath.Join(root, "log"),
		"-o", "-h 127.0.0.1 -p "+port+" -k "+root+" -F", "start"); err != nil {
		return nil, err
	}
	if fresh {
		if err := s.run(ctx, "createdb", "-h", "127.0.0.1", "-p", port, "-U", "app", "app"); err != nil {
			s.Stop()
			return nil, err
		}
	}
	s.URL = "postgres://app:app@127.0.0.1:" + port + "/app"
	return s, nil
}

func (s *Server) run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), s.env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		o := strings.TrimSpace(out.String())
		if len(o) > 1000 {
			o = o[len(o)-1000:]
		}
		return fmt.Errorf("%s: %w: %s", name, err, o)
	}
	return nil
}

func (s *Server) Stop() {
	cmd := exec.Command("pg_ctl", "-D", filepath.Join(s.root, "data"), "-m", "fast", "stop")
	cmd.Env = append(os.Environ(), s.env...)
	_ = cmd.Run()
	if s.temp {
		os.RemoveAll(s.root)
	}
}

// nssWrapper is where Debian's libnss-wrapper lives; the images have it.
var nssWrapper = []string{"/usr/lib/x86_64-linux-gnu/libnss_wrapper.so", "/usr/lib/aarch64-linux-gnu/libnss_wrapper.so"}

// passwdFor lets PostgreSQL run as a user id without an /etc/passwd
// entry, as in a container started with --user to match the owner of a
// mounted folder: initdb refuses such a user. nss_wrapper hands the
// PostgreSQL programs a passwd file with that id in it.
func passwdFor(dir string) ([]string, error) {
	if _, err := user.Current(); err == nil {
		return nil, nil
	}
	lib := ""
	for _, l := range nssWrapper {
		if _, err := os.Stat(l); err == nil {
			lib = l
		}
	}
	if lib == "" {
		return nil, fmt.Errorf("user id %d has no passwd entry, which PostgreSQL needs; run as a known user or set DATABASE_URL", os.Getuid())
	}
	uid, gid := os.Getuid(), os.Getgid()
	passwd := filepath.Join(dir, "passwd")
	group := filepath.Join(dir, "group")
	if err := os.WriteFile(passwd, []byte(fmt.Sprintf("kapelo:x:%d:%d:kapelo:%s:/bin/sh\n", uid, gid, os.TempDir())), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(group, []byte(fmt.Sprintf("kapelo:x:%d:\n", gid)), 0o644); err != nil {
		return nil, err
	}
	return []string{"LD_PRELOAD=" + lib, "NSS_WRAPPER_PASSWD=" + passwd, "NSS_WRAPPER_GROUP=" + group}, nil
}
