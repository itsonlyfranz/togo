package session_test

import (
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/gettogo/togo/helper/internal/herdr"
	"github.com/gettogo/togo/helper/internal/session"
)

// TestLiveHerdrService runs sessions through Service on a real Herdr
// server. It runs only when TOGO_HERDR_LIVE_SESSION names a dedicated,
// already running session, and it refuses "default": it creates, renames,
// reads and kills sessions there. TOGO_HERDR_LIVE_AGENT, when set, is an
// installed agent such as claude to start in one of them as well.
// TOGO_HERDR_LIVE_BIN overrides the herdr binary.
func TestLiveHerdrService(t *testing.T) {
	name := os.Getenv("TOGO_HERDR_LIVE_SESSION")
	if name == "" {
		t.Skip("TOGO_HERDR_LIVE_SESSION not set")
	}
	if name == "default" {
		t.Fatal("refusing to run against the default Herdr session")
	}
	client := &herdr.Client{Bin: cmp.Or(os.Getenv("TOGO_HERDR_LIVE_BIN"), "herdr"), Session: name}
	svc, _ := herdrService(t, client, os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	s := mustCreate(t, svc, session.CreateOpts{Name: "togo live"})
	t.Cleanup(func() { _ = svc.Kill(context.Background(), s.ID) })
	if list := mustList(t, svc); !slices.ContainsFunc(list, func(x session.Session) bool { return x.ID == s.ID && x.Name == "togo live" }) {
		t.Fatalf("List = %+v, want %s", list, s.ID)
	}
	if _, err := svc.Rename(ctx, s.ID, "togo renamed"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	ws, err := client.ListWorkspaces(ctx)
	if err != nil || !slices.ContainsFunc(ws, func(w herdr.Workspace) bool { return w.Label == "togo renamed" }) {
		t.Fatalf("workspaces = %+v, %v; want one labeled togo renamed", ws, err)
	}
	argv, err := svc.AttachCommand(ctx, s.ID)
	if err != nil || len(argv) < 4 || argv[1] != "terminal" || argv[2] != "attach" || !slices.Contains(argv, name) {
		t.Fatalf("AttachCommand = %v, %v; want a terminal attach on %s", argv, err, name)
	}
	if gone, err := svc.AgentGone(ctx, s.ID); err != nil || !gone {
		t.Fatalf("AgentGone of the shell = %v, %v; want gone", gone, err)
	}
	if _, _, err := svc.Capture(ctx, s.ID, 50, true); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if agent := os.Getenv("TOGO_HERDR_LIVE_AGENT"); agent != "" {
		a := mustCreate(t, svc, session.CreateOpts{Agent: agent})
		t.Cleanup(func() { _ = svc.Kill(context.Background(), a.ID) })
		if gone, err := svc.AgentGone(ctx, a.ID); err != nil || gone {
			t.Fatalf("AgentGone of %s = %v, %v; want it running", agent, gone, err)
		}
		// A fresh folder holds the agent at its trust dialog (claude does),
		// where Herdr refuses a prompt before typing anything.
		var refused *session.ReplyRefusedError
		if err := svc.SendText(ctx, a.ID, "hello", true, "", nil); err != nil && !errors.As(err, &refused) {
			t.Fatalf("SendText to %s = %v, want it sent or refused", agent, err)
		}
		if _, err := svc.Relaunch(ctx, a.ID); !errors.Is(err, session.ErrRunning) {
			t.Fatalf("Relaunch of a running %s = %v, want ErrRunning", agent, err)
		}
		if err := svc.Kill(ctx, a.ID); err != nil {
			t.Fatalf("Kill %s: %v", agent, err)
		}
	}

	if err := svc.Kill(ctx, s.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if list := mustList(t, svc); slices.ContainsFunc(list, func(x session.Session) bool { return x.ID == s.ID }) {
		t.Fatalf("List after Kill = %+v", list)
	}
}
