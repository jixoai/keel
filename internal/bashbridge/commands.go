package bashbridge

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/ewhauser/gbash/commands"
)

// Command is one command injected into the sandbox registry. gbash's registry
// replaces an existing command with the same name, so an injected "jobs" would
// shadow the (unimplemented) upstream builtin; keel.creation/jobs semantics are
// still an explicit decision, not an accident. The bridge never exposes gbash's
// commands.Invocation type.
type Command struct {
	Name string
	Run  func(ctx context.Context, inv Invocation) error
}

// Invocation is what an injected command receives. File access goes through FS
// (gbash's policy-checked CommandFS), never the host; network access is not
// offered because the sandbox has none.
type Invocation struct {
	Args   []string
	Env    map[string]string
	Cwd    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	FS     SandboxFS
}

// SandboxFS is the policy-checked file view an injected command may use. It is
// intentionally small: the operations a keel command needs, expressed in
// stdlib types. Paths are sandbox paths (/workspace/...), not host paths.
type SandboxFS interface {
	ReadFile(ctx context.Context, name string) ([]byte, error)
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	Stat(ctx context.Context, name string) (fs.FileInfo, error)
	ReadDir(ctx context.Context, name string) ([]fs.DirEntry, error)
	MkdirAll(ctx context.Context, name string, perm fs.FileMode) error
	Remove(ctx context.Context, name string) error
}

// registerCommands installs the injected commands on a fresh default registry.
func registerCommands(registry *commands.Registry, injected []Command) error {
	for _, command := range injected {
		if command.Name == "" {
			return fmt.Errorf("bashbridge: injected command has no name")
		}
		if command.Run == nil {
			return fmt.Errorf("bashbridge: injected command %q has no Run function", command.Name)
		}
		current := command
		if err := registry.Register(commands.DefineCommand(current.Name, func(ctx context.Context, inv *commands.Invocation) error {
			return current.Run(ctx, Invocation{
				Args:   append([]string(nil), inv.Args...),
				Env:    inv.Env,
				Cwd:    inv.Cwd,
				Stdin:  inv.Stdin,
				Stdout: inv.Stdout,
				Stderr: inv.Stderr,
				FS:     commandFS{fs: inv.FS},
			})
		})); err != nil {
			return fmt.Errorf("bashbridge: register command %q: %w", current.Name, err)
		}
	}
	return nil
}

// commandFS adapts gbash's *commands.CommandFS to SandboxFS.
type commandFS struct {
	fs *commands.CommandFS
}

func (c commandFS) ReadFile(ctx context.Context, name string) ([]byte, error) {
	if c.fs == nil {
		return nil, fs.ErrInvalid
	}
	return c.fs.ReadFile(ctx, name)
}

func (c commandFS) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	if c.fs == nil {
		return fs.ErrInvalid
	}
	file, err := c.fs.OpenFile(ctx, name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func (c commandFS) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	if c.fs == nil {
		return nil, fs.ErrInvalid
	}
	return c.fs.Stat(ctx, name)
}

func (c commandFS) ReadDir(ctx context.Context, name string) ([]fs.DirEntry, error) {
	if c.fs == nil {
		return nil, fs.ErrInvalid
	}
	return c.fs.ReadDir(ctx, name)
}

func (c commandFS) MkdirAll(ctx context.Context, name string, perm fs.FileMode) error {
	if c.fs == nil {
		return fs.ErrInvalid
	}
	return c.fs.MkdirAll(ctx, name, perm)
}

func (c commandFS) Remove(ctx context.Context, name string) error {
	if c.fs == nil {
		return fs.ErrInvalid
	}
	return c.fs.Remove(ctx, name, false)
}
