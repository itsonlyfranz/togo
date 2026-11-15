package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gettogo/togo/helper/internal/herdr"
	"github.com/gettogo/togo/helper/internal/wire"
)

// HerdrUnavailableError means Herdr cannot run sessions: it is not
// installed, its server is not running, or the server is a version this
// herdr binary cannot drive.
type HerdrUnavailableError struct {
	Reason string
}

// Error names the reason.
func (e *HerdrUnavailableError) Error() string { return "herdr is unavailable: " + e.Reason }

// HerdrClient is the part of *herdr.Client the Herdr backend uses.
type HerdrClient interface {
	Status(ctx context.Context) (herdr.ServerStatus, error)
	ListWorkspaces(ctx context.Context) ([]herdr.Workspace, error)
	ListPanes(ctx context.Context, workspaceID string) ([]herdr.Pane, error)
	CreateWorkspace(ctx context.Context, o herdr.CreateOpts) (herdr.Created, error)
	RenameWorkspace(ctx context.Context, id, label string) (herdr.Workspace, error)
	CloseWorkspace(ctx context.Context, id string) error
	ReadPane(ctx context.Context, paneID string, o herdr.ReadOpts) (string, error)
	SendText(ctx context.Context, paneID, text string) error
	SendKeys(ctx context.Context, paneID string, keys ...string) error
	StartAgent(ctx context.Context, o herdr.StartOpts) error
	PromptAgent(ctx context.Context, target, text string) error
	PaneProcesses(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
	AttachArgv(terminalID string) []string
}

// herdrKinds are the registry agents Herdr can start itself, by kind. A
// session for any other agent cannot run on Herdr.
var herdrKinds = map[string]bool{
	"amp": true, "claude": true, "cline": true, "codex": true, "copilot": true,
	"cursor": true, "droid": true, "kilo": true, "kimi": true, "kiro": true,
	"omp": true, "opencode": true, "pi": true,
}

// HerdrBackend runs each session as a workspace of one Herdr session,
// whose root pane is the terminal. The shell starts first and the agent is
// started in it with "herdr agent start", so Herdr recognizes it. Herdr
// keeps no values for togo, so they live in a JSON file next to the
// workspace and pane they belong to.
//
// Herdr cannot run a command in place of the shell, change a running
// pane's environment, or stop an agent for a restart, so the agent's exit
// status is not reported, SetEnv reaches no pane, and Respawn only starts
// an agent whose pane is back at its shell.
type HerdrBackend struct {
	Client HerdrClient
	// Store is the JSON file of session values; LockFile serializes its
	// writers across processes.
	Store    string
	LockFile string
	// StartTimeout bounds the wait for a started agent to be ready; zero
	// means 15 seconds.
	StartTimeout time.Duration
}

// herdrAgentName is the Herdr agent name of a session's agent. Session
// ids already match Herdr's name pattern.
func herdrAgentName(id string) string { return id }

func (b *HerdrBackend) Check(ctx context.Context) (bool, error) {
	st, err := b.Client.Status(ctx)
	switch {
	case errors.Is(err, herdr.ErrNotInstalled):
		return true, &HerdrUnavailableError{Reason: "herdr is not installed"}
	case err != nil:
		return false, err
	case !st.Running:
		return false, &HerdrUnavailableError{Reason: "the herdr server is not running"}
	case !st.Compatible:
		return false, &HerdrUnavailableError{Reason: fmt.Sprintf("the herdr server (%s) is not compatible with this herdr", st.Version)}
	}
	return false, nil
}

// List matches the stored sessions against the live workspaces. A session
// whose workspace or root pane is gone was closed in Herdr, and its values
// are dropped; with no server running, nothing is dropped.
func (b *HerdrBackend) List(ctx context.Context) ([]Record, error) {
	var out []Record
	err := b.update(ctx, func(entries []herdrEntry) ([]herdrEntry, error) {
		live, up, err := b.livePanes(ctx)
		if err != nil || !up {
			return entries, err
		}
		kept := entries[:0]
		for _, e := range entries {
			p, ok := live[e.Pane]
			if !ok || p.WorkspaceID != e.Workspace || p.TerminalID != e.Terminal {
				continue
			}
			kept = append(kept, e)
			out = append(out, Record{ID: e.ID, Created: e.Created, Path: cmp.Or(p.Cwd, e.Dir), Values: maps.Clone(e.Values)})
		}
		return kept, nil
	})
	return out, err
}

// livePanes returns every pane by id, and false when no server runs.
func (b *HerdrBackend) livePanes(ctx context.Context) (map[string]herdr.Pane, bool, error) {
	ws, err := b.Client.ListWorkspaces(ctx)
	if err != nil || ws == nil {
		return nil, false, err
	}
	panes, err := b.Client.ListPanes(ctx, "")
	if err != nil || panes == nil {
		return nil, false, err
	}
	live := make(map[string]herdr.Pane, len(panes))
	for _, p := range panes {
		live[p.ID] = p
	}
	return live, true, nil
}

func (b *HerdrBackend) Start(ctx context.Context, spec StartSpec) error {
	launch := spec.Run.Launch
	if launch.Program != "" && !herdrKinds[spec.Run.Agent] {
		return &InvalidParamsError{Reason: wire.ReasonProgram, Msg: fmt.Sprintf("agent %q cannot run on Herdr", spec.Run.Agent)}
	}
	created, err := b.Client.CreateWorkspace(ctx, herdr.CreateOpts{
		Label: cmp.Or(spec.Values[optionName], spec.ID), Dir: spec.Dir, Env: spec.Env,
	})
	if err != nil {
		return err
	}
	e := herdrEntry{
		ID: spec.ID, Workspace: created.Workspace.ID, Pane: created.RootPane.ID, Terminal: created.RootPane.TerminalID,
		Dir: spec.Dir, Created: time.Now().UTC().Truncate(time.Second), Values: maps.Clone(spec.Values),
	}
	err = b.update(ctx, func(entries []herdrEntry) ([]herdrEntry, error) {
		return append(entries, e), nil
	})
	if err == nil && launch.Program != "" {
		err = b.startAgent(ctx, e, spec.Run)
	}
	if err != nil {
		// The workspace is the caller's only if Start succeeds.
		_ = b.Client.CloseWorkspace(context.WithoutCancel(ctx), e.Workspace)
		_ = b.remove(context.WithoutCancel(ctx), e.ID)
		return err
	}
	return nil
}

// startAgent starts run's agent in the session's pane. An agent held at a
// startup dialog is running, waiting for the user, so that is success.
func (b *HerdrBackend) startAgent(ctx context.Context, e herdrEntry, run Run) error {
	if !herdrKinds[run.Agent] {
		return &InvalidParamsError{Reason: wire.ReasonProgram, Msg: fmt.Sprintf("agent %q cannot run on Herdr", run.Agent)}
	}
	err := b.Client.StartAgent(ctx, herdr.StartOpts{
		Name: herdrAgentName(e.ID), Kind: run.Agent, PaneID: e.Pane,
		Args: run.Launch.Args, Timeout: cmp.Or(b.StartTimeout, 15*time.Second),
	})
	if err != nil && !errors.Is(err, herdr.ErrAgentNotReady) {
		return fmt.Errorf("herdr agent start: %w", err)
	}
	return nil
}

func (b *HerdrBackend) Kill(ctx context.Context, id string) error {
	e, err := b.entry(ctx, id)
	if err != nil {
		return err
	}
	err = b.Client.CloseWorkspace(ctx, e.Workspace)
	if err == nil || errors.Is(err, herdr.ErrNotFound) {
		if rmErr := b.remove(ctx, id); rmErr != nil {
			return rmErr
		}
	}
	return err
}

// SetValue stores a value; a new name also becomes the workspace label.
func (b *HerdrBackend) SetValue(ctx context.Context, id, key, value string) error {
	e, err := b.entry(ctx, id)
	if err != nil {
		return err
	}
	if key == optionName {
		if _, err := b.Client.RenameWorkspace(ctx, e.Workspace, value); err != nil {
			return err
		}
	}
	return b.setValue(ctx, id, key, &value)
}

func (b *HerdrBackend) UnsetValue(ctx context.Context, id, key string) error {
	return b.setValue(ctx, id, key, nil)
}

func (b *HerdrBackend) setValue(ctx context.Context, id, key string, value *string) error {
	return b.update(ctx, func(entries []herdrEntry) ([]herdrEntry, error) {
		for i := range entries {
			if entries[i].ID != id {
				continue
			}
			if entries[i].Values == nil {
				entries[i].Values = map[string]string{}
			}
			if value == nil {
				delete(entries[i].Values, key)
			} else {
				entries[i].Values[key] = *value
			}
			return entries, nil
		}
		return entries, fmt.Errorf("%w: %s", ErrNotFound, id)
	})
}

// SetEnv only checks the session exists: a Herdr pane's environment is
// fixed when the workspace is created.
func (b *HerdrBackend) SetEnv(ctx context.Context, id, _, _ string) error {
	_, err := b.entry(ctx, id)
	return err
}

// AttachArgv attaches a terminal straight to the session's pane, as a
// live client that takes input and draws output.
func (b *HerdrBackend) AttachArgv(ctx context.Context, id string) ([]string, error) {
	e, err := b.entry(ctx, id)
	if err != nil {
		return nil, err
	}
	return b.Client.AttachArgv(e.Terminal), nil
}

func (b *HerdrBackend) Capture(ctx context.Context, id string, lines int, join bool) (string, error) {
	e, err := b.entry(ctx, id)
	if err != nil {
		return "", err
	}
	source := herdr.SourceRecent
	if join {
		source = herdr.SourceRecentUnwrapped
	}
	return b.Client.ReadPane(ctx, e.Pane, herdr.ReadOpts{Source: source, Lines: lines})
}

// Reply submits through the agent surface, which pastes and presses Enter
// as one ordered write, so there is no settle wait. Text is otherwise typed
// as keystrokes, not pasted, so a newline in it would press Enter: that is
// typing without submit, and text starting with "-", which agent prompt
// would read as an option. Multi-line text is refused in both.
func (b *HerdrBackend) Reply(ctx context.Context, id, text string, submit bool) error {
	e, err := b.entry(ctx, id)
	if err != nil {
		return err
	}
	if !submit || strings.HasPrefix(text, "-") {
		if strings.Contains(text, "\n") {
			return &InvalidParamsError{Reason: wire.ReasonText, Msg: "this text cannot be typed into a Herdr session in one piece"}
		}
		if err := b.Client.SendText(ctx, e.Pane, text); err != nil || !submit {
			return err
		}
		return b.Client.SendKeys(ctx, e.Pane, "enter")
	}
	err = b.Client.PromptAgent(ctx, e.Pane, text)
	switch {
	case errors.Is(err, herdr.ErrAgentBlocked):
		return &ReplyRefusedError{Reason: wire.ConflictNeedsYou}
	case errors.Is(err, herdr.ErrNoAgent):
		return &ReplyRefusedError{Reason: wire.ConflictEnded}
	}
	return err
}

func (b *HerdrBackend) ShellInForeground(ctx context.Context, id string) (bool, error) {
	e, err := b.entry(ctx, id)
	if err != nil {
		return false, err
	}
	info, err := b.Client.PaneProcesses(ctx, e.Pane)
	if err != nil {
		return false, err
	}
	return info.ShellInForeground(), nil
}

// AgentGone is ShellInForeground: with no wrapper around the agent, the
// pane's shell alone in the foreground means it has exited.
func (b *HerdrBackend) AgentGone(ctx context.Context, id string) (bool, error) {
	return b.ShellInForeground(ctx, id)
}

// Respawn starts the agent again in the pane's shell, in whatever folder
// the shell is in. Herdr cannot stop a running agent, so one still running
// is ErrRunning.
func (b *HerdrBackend) Respawn(ctx context.Context, id, _ string, run Run) error {
	gone, err := b.AgentGone(ctx, id)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("%w: %s", ErrRunning, id)
	}
	e, err := b.entry(ctx, id)
	if err != nil {
		return err
	}
	return b.startAgent(ctx, e, run)
}

// ServerStarted reports whether the server runs; Herdr does not say when
// it started.
func (b *HerdrBackend) ServerStarted(ctx context.Context) (time.Time, bool, error) {
	st, err := b.Client.Status(ctx)
	if errors.Is(err, herdr.ErrNotInstalled) {
		return time.Time{}, false, nil
	}
	return time.Time{}, st.Running, err
}
