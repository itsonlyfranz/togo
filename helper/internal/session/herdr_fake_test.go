package session_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gettogo/togo/helper/internal/herdr"
	"github.com/gettogo/togo/helper/internal/session"
	"github.com/gettogo/togo/helper/internal/state"
)

// fakeHerdr is an in-memory Herdr server for the Herdr backend. Each
// workspace has one root pane; a pane's shell is in the foreground until an
// agent starts in it.
type fakeHerdr struct {
	mu       sync.Mutex
	down     bool
	n        int
	labels   map[string]string // workspace id to label
	panes    map[string]herdr.Pane
	envs     map[string]map[string]string // workspace id to its create env
	agentIn  map[string]bool              // pane id to an agent running
	started  []herdr.StartOpts
	prompted []string
	typed    []string
	keys     []string
	closed   []string

	startErr, promptErr error
}

func newFakeHerdr() *fakeHerdr {
	return &fakeHerdr{labels: map[string]string{}, panes: map[string]herdr.Pane{}, envs: map[string]map[string]string{}, agentIn: map[string]bool{}}
}

// herdrService returns a Service whose sessions run on Herdr through c,
// with its own state directory and agents found only in bin.
func herdrService(t *testing.T, c session.HerdrClient, bin string) (*session.Service, *session.HerdrBackend) {
	t.Helper()
	dir := t.TempDir()
	b := session.NewHerdrBackend(dir, c)
	return &session.Service{
		Backend:  b,
		Recent:   &state.Recent{File: filepath.Join(dir, "recent.json"), Keeps: func(string) bool { return true }},
		LockFile: filepath.Join(dir, "run", "bib.lock"),
		Manifest: &session.Manifest{Path: filepath.Join(dir, "sessions.json")},
		Path:     bin,
		Now:      func() time.Time { return fixedNow },
	}, b
}

func (f *fakeHerdr) Status(context.Context) (herdr.ServerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return herdr.ServerStatus{}, nil
	}
	return herdr.ServerStatus{Running: true, Version: "0.9.5", Compatible: true}, nil
}

func (f *fakeHerdr) ListWorkspaces(context.Context) ([]herdr.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, nil
	}
	out := []herdr.Workspace{}
	for id, label := range f.labels {
		out = append(out, herdr.Workspace{ID: id, Label: label})
	}
	return out, nil
}

func (f *fakeHerdr) ListPanes(_ context.Context, ws string) ([]herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, nil
	}
	out := []herdr.Pane{}
	for _, p := range f.panes {
		if ws == "" || p.WorkspaceID == ws {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeHerdr) CreateWorkspace(_ context.Context, o herdr.CreateOpts) (herdr.Created, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return herdr.Created{}, &herdr.APIError{Code: "server_not_running", Message: "down"}
	}
	f.n++
	ws := fmt.Sprintf("w%d", f.n)
	p := herdr.Pane{ID: ws + ":p1", WorkspaceID: ws, TabID: ws + ":t1", TerminalID: fmt.Sprintf("term_%d", f.n), Cwd: o.Dir}
	f.labels[ws], f.panes[p.ID], f.envs[ws] = o.Label, p, o.Env
	return herdr.Created{Workspace: herdr.Workspace{ID: ws, Label: o.Label}, TabID: p.TabID, RootPane: p}, nil
}

func (f *fakeHerdr) RenameWorkspace(_ context.Context, id, label string) (herdr.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.labels[id]; !ok {
		return herdr.Workspace{}, &herdr.APIError{Code: "workspace_not_found", Message: id}
	}
	f.labels[id] = label
	return herdr.Workspace{ID: id, Label: label}, nil
}

func (f *fakeHerdr) CloseWorkspace(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	if _, ok := f.labels[id]; !ok {
		return &herdr.APIError{Code: "workspace_not_found", Message: id}
	}
	f.closeLocked(id)
	return nil
}

// closeInHerdr closes a workspace the way the user would in Herdr.
func (f *fakeHerdr) closeInHerdr(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeLocked(id)
}

func (f *fakeHerdr) closeLocked(id string) {
	delete(f.labels, id)
	for pid, p := range f.panes {
		if p.WorkspaceID == id {
			delete(f.panes, pid)
		}
	}
}

func (f *fakeHerdr) pane(id string) error {
	if _, ok := f.panes[id]; !ok {
		return &herdr.APIError{Code: "pane_not_found", Message: id}
	}
	return nil
}

func (f *fakeHerdr) ReadPane(_ context.Context, id string, o herdr.ReadOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.pane(id); err != nil {
		return "", err
	}
	return fmt.Sprintf("screen of %s from %s, %d lines\n\n", id, o.Source, o.Lines), nil
}

func (f *fakeHerdr) SendText(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typed = append(f.typed, text)
	return f.pane(id)
}

func (f *fakeHerdr) SendKeys(_ context.Context, id string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, keys...)
	return f.pane(id)
}

func (f *fakeHerdr) StartAgent(_ context.Context, o herdr.StartOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, o)
	if err := f.pane(o.PaneID); err != nil {
		return err
	}
	if f.startErr != nil {
		return f.startErr
	}
	f.agentIn[o.PaneID] = true
	return nil
}

func (f *fakeHerdr) PromptAgent(_ context.Context, target, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompted = append(f.prompted, target+" "+text)
	return f.promptErr
}

func (f *fakeHerdr) PaneProcesses(_ context.Context, id string) (herdr.ProcessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.pane(id); err != nil {
		return herdr.ProcessInfo{}, err
	}
	info := herdr.ProcessInfo{ShellPID: 100, Foreground: []herdr.Process{{PID: 100, Argv0: "zsh"}}}
	if f.agentIn[id] {
		info.Foreground = []herdr.Process{{PID: 200, Argv0: "claude"}}
	}
	return info, nil
}

// exitAgent ends the agent in a pane, leaving its shell.
func (f *fakeHerdr) exitAgent(pane string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentIn[pane] = false
}

func (f *fakeHerdr) AttachArgv(terminal string) []string {
	return []string{"herdr", "terminal", "attach", terminal, "--session", "fake"}
}
