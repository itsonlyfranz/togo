package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gettogo/togo/helper/internal/herdr"
	"github.com/gettogo/togo/helper/internal/tmux"
)

// Backend is where managed sessions run: it starts their terminals, keeps
// the values togo stores with each one (bib, name, agent, argv, exit and
// the like, under the option keys in create.go) and reaches into their
// panes. Service checks every input before it calls a Backend. A missing
// session is an error that wraps ErrNotFound, tmux.ErrNoSession or
// herdr.ErrNotFound.
type Backend interface {
	// Check reports whether sessions can run now. stable means the answer
	// holds while the helper runs, so Service keeps it.
	Check(ctx context.Context) (stable bool, err error)
	// List returns the managed sessions. With no server running it returns
	// none and no error.
	List(ctx context.Context) ([]Record, error)
	// Start starts the session described by spec.
	Start(ctx context.Context, spec StartSpec) error
	// Kill ends the session and everything running in it.
	Kill(ctx context.Context, id string) error
	// SetValue and UnsetValue change one stored value.
	SetValue(ctx context.Context, id, key, value string) error
	UnsetValue(ctx context.Context, id, key string) error
	// SetEnv sets a variable for programs the session starts from now on.
	SetEnv(ctx context.Context, id, key, value string) error
	// AttachArgv is the argv that attaches a terminal to the session.
	AttachArgv(ctx context.Context, id string) ([]string, error)
	// Capture is the text of lines rows of history and the screen; join
	// rejoins soft-wrapped rows.
	Capture(ctx context.Context, id string, lines int, join bool) (string, error)
	// Reply types validated reply text into the agent and, with submit,
	// presses Enter.
	Reply(ctx context.Context, id, text string, submit bool) error
	// ShellInForeground reports whether a shell, not an agent, is in the
	// foreground of the session's pane, where typed text would run.
	ShellInForeground(ctx context.Context, id string) (bool, error)
	// AgentGone reports whether no agent runs in the session's pane.
	AgentGone(ctx context.Context, id string) (bool, error)
	// Respawn runs the agent of run again in the session's pane, in dir.
	Respawn(ctx context.Context, id, dir string, run Run) error
	// ServerStarted reports when the server holding the sessions started
	// and whether one is running; a zero time means it is not known.
	ServerStarted(ctx context.Context) (time.Time, bool, error)
}

// Record is a managed session as a Backend lists it. Values holds what
// was stored under the option keys.
type Record struct {
	ID       string
	Created  time.Time
	Attached int
	Dead     bool
	Path     string
	Values   map[string]string
}

// Run is the agent a session runs: the registry id and its Launch. The
// zero Launch is the plain shell.
type Run struct {
	Agent  string
	Launch Launch
}

// StartSpec describes a session for Backend.Start.
type StartSpec struct {
	ID         string
	Dir        string
	Cols, Rows int
	Run        Run
	// Values are stored with the session from the start.
	Values map[string]string
	// Env is the environment of the session's pane.
	Env map[string]string
}

// backend returns the Backend sessions run on: Backend when set, else
// tmux on the togo socket.
func (s *Service) backend() Backend {
	if s.Backend != nil {
		return s.Backend
	}
	return &tmuxBackend{c: s.Tmux, shell: s.shell(), togoPath: s.TogoPath}
}

// isNoSession reports whether err says the session does not exist.
func isNoSession(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, tmux.ErrNoSession) || errors.Is(err, herdr.ErrNotFound)
}

// tmuxBackend runs sessions on a tmux server, storing values as session
// user options.
type tmuxBackend struct {
	c        *tmux.Client
	shell    string
	togoPath string
}

func (b *tmuxBackend) Check(ctx context.Context) (bool, error) {
	major, minor, raw, err := b.c.Version(ctx)
	switch {
	case errors.Is(err, tmux.ErrNotInstalled):
		return true, &TmuxUnavailableError{}
	case err != nil && raw == "":
		return false, fmt.Errorf("tmux version: %w", err)
	case err != nil || major < requiredMajor || major == requiredMajor && minor < requiredMinor:
		return true, &TmuxUnavailableError{Found: trimTmux(raw)}
	}
	return true, nil
}

func (b *tmuxBackend) List(ctx context.Context) ([]Record, error) {
	infos, err := b.c.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(infos))
	for _, info := range infos {
		values := map[string]string{
			optionBib: info.Bib, optionAgent: info.Agent, optionName: info.Title,
			optionCreated: info.CreatedTag, optionArgv: info.Argv, optionExit: info.Exit,
		}
		if info.Muted {
			values[optionMuted] = "1"
		}
		out = append(out, Record{ID: info.Name, Created: info.Created, Attached: info.Attached, Dead: info.Dead, Path: info.Path, Values: values})
	}
	return out, nil
}

func (b *tmuxBackend) Start(ctx context.Context, spec StartSpec) error {
	return b.c.NewSession(ctx, tmux.NewSessionOpts{
		Name:    spec.ID,
		Dir:     spec.Dir,
		Cols:    spec.Cols,
		Rows:    spec.Rows,
		Command: launchWrapper(b.shell, b.togoPath, spec.Run.Launch),
		Options: spec.Values,
		Env:     spec.Env,
	})
}

func (b *tmuxBackend) Kill(ctx context.Context, id string) error {
	return b.c.KillSession(ctx, id)
}

func (b *tmuxBackend) SetValue(ctx context.Context, id, key, value string) error {
	return b.c.SetOption(ctx, id, key, value)
}

func (b *tmuxBackend) UnsetValue(ctx context.Context, id, key string) error {
	return b.c.UnsetOption(ctx, id, key)
}

func (b *tmuxBackend) SetEnv(ctx context.Context, id, key, value string) error {
	return b.c.SetEnvironment(ctx, id, key, value)
}

func (b *tmuxBackend) AttachArgv(_ context.Context, id string) ([]string, error) {
	return b.c.AttachArgv(id), nil
}

func (b *tmuxBackend) Capture(ctx context.Context, id string, lines int, join bool) (string, error) {
	return b.c.CaptureHistory(ctx, id, lines, join)
}

func (b *tmuxBackend) Reply(ctx context.Context, id, text string, submit bool) error {
	return typeReply(ctx, b.c, id, text, submit, sleepCtx)
}

func (b *tmuxBackend) ShellInForeground(ctx context.Context, id string) (bool, error) {
	cmd, err := b.c.PaneCommand(ctx, "="+id+":")
	if err != nil {
		return false, err
	}
	return isShell(cmd, b.shell), nil
}

func (b *tmuxBackend) AgentGone(ctx context.Context, id string) (bool, error) {
	fg, err := b.c.Foreground(ctx, id)
	if err != nil {
		return false, err
	}
	return AgentGone(fg, b.togoPath), nil
}

func (b *tmuxBackend) Respawn(ctx context.Context, id, dir string, run Run) error {
	return b.c.RespawnPane(ctx, "="+id+":", dir, launchWrapper(b.shell, b.togoPath, run.Launch))
}

func (b *tmuxBackend) ServerStarted(ctx context.Context) (time.Time, bool, error) {
	return b.c.ServerStarted(ctx)
}
