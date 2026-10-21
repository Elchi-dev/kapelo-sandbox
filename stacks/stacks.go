// Package stacks holds the starter stacks that ship with the worker. A
// stack is a folder with stack.json: how to scaffold a project, the rules
// a worker follows in it, and the checks its work must pass.
package stacks

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

//go:embed */*
var files embed.FS

// Names lists the stacks that ship with the worker.
func Names() []string {
	entries, _ := fs.ReadDir(files, ".")
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// Extract writes a shipped stack into dir/name and returns that folder,
// so its scripts can run from disk.
func Extract(name, dir string) (string, error) {
	if !slices.Contains(Names(), name) {
		return "", fmt.Errorf("no stack %q ships with the worker; there are %v", name, Names())
	}
	dest := filepath.Join(dir, name)
	err := fs.WalkDir(files, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if filepath.Ext(p) == ".sh" {
			mode = 0o755
		}
		return os.WriteFile(target, b, mode)
	})
	return dest, err
}
