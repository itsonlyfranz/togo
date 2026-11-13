package herdr_test

import (
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gettogo/togo/helper/internal/herdr"
)

// TestLiveWorkspaceLifecycle drives a real Herdr server. It runs only when
// TOGO_HERDR_LIVE_SESSION names a dedicated, already running session, and
// it refuses "default": it creates, types into and closes a workspace there.
// TOGO_HERDR_LIVE_BIN overrides the herdr binary.
func TestLiveWorkspaceLifecycle(t *testing.T) {
	session := os.Getenv("TOGO_HERDR_LIVE_SESSION")
	if session == "" {
		t.Skip("TOGO_HERDR_LIVE_SESSION not set")
	}
	if session == "default" {
		t.Fatal("refusing to run against the default Herdr session")
	}
	c := &herdr.Client{Bin: cmp.Or(os.Getenv("TOGO_HERDR_LIVE_BIN"), "herdr"), Session: session}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	st, err := c.Status(ctx)
	if err != nil || !st.Running {
		t.Fatalf("Status = %+v, %v; want a running server", st, err)
	}

	dir := t.TempDir()
	const label = `togo live "x"; $(y)`
	created, err := c.CreateWorkspace(ctx, herdr.CreateOpts{
		Label: label, Dir: dir, Env: map[string]string{"TOGO_LIVE": "a b"},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	id := created.Workspace.ID
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = c.CloseWorkspace(context.Background(), id)
		}
	})
	if created.Workspace.Label != label || created.RootPane.WorkspaceID != id || created.RootPane.TerminalID == "" {
		t.Fatalf("Created = %+v, want label %q and a root pane with a terminal in %s", created, label, id)
	}

	ws, err := c.ListWorkspaces(ctx)
	if err != nil || !slices.ContainsFunc(ws, func(w herdr.Workspace) bool { return w.ID == id }) {
		t.Fatalf("ListWorkspaces = %+v, %v; want it to include %s", ws, err, id)
	}
	panes, err := c.ListPanes(ctx, id)
	if err != nil || len(panes) != 1 || panes[0].ID != created.RootPane.ID {
		t.Fatalf("ListPanes(%s) = %+v, %v; want just the root pane", id, panes, err)
	}

	pane := created.RootPane.ID
	if err := c.SendText(ctx, pane, `echo "live-[$TOGO_LIVE]"`); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if err := c.SendKeys(ctx, pane, "enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	var screen string
	for !strings.Contains(screen, "\nlive-[a b]") {
		if ctx.Err() != nil {
			t.Fatalf("the echo never appeared; last read:\n%s", screen)
		}
		time.Sleep(100 * time.Millisecond)
		if screen, err = c.ReadPane(ctx, pane, herdr.ReadOpts{Source: herdr.SourceRecentUnwrapped, Lines: 50}); err != nil {
			t.Fatalf("ReadPane: %v", err)
		}
	}

	if err := c.SendKeys(ctx, pane, "no-such-key"); !errors.As(err, new(*herdr.APIError)) {
		t.Fatalf("SendKeys(no-such-key) = %v, want an APIError", err)
	}

	renamed, err := c.RenameWorkspace(ctx, id, "-renamed")
	if err != nil || renamed.Label != "-renamed" {
		t.Fatalf("RenameWorkspace = %+v, %v; want label -renamed", renamed, err)
	}

	if err := c.CloseWorkspace(ctx, id); err != nil {
		t.Fatalf("CloseWorkspace: %v", err)
	}
	closed = true
	if err := c.CloseWorkspace(ctx, id); !errors.Is(err, herdr.ErrNotFound) {
		t.Fatalf("second CloseWorkspace = %v, want ErrNotFound", err)
	}
	if _, err := c.ReadPane(ctx, pane, herdr.ReadOpts{}); !errors.Is(err, herdr.ErrNotFound) {
		t.Fatalf("ReadPane after close = %v, want ErrNotFound", err)
	}
}
