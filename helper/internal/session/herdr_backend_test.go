package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gettogo/togo/helper/internal/herdr"
	"github.com/gettogo/togo/helper/internal/session"
	"github.com/gettogo/togo/helper/internal/wire"
)

func TestHerdrCreateRunsTheShellInAWorkspaceAndListsIt(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, "")
	dir := realpath(t, t.TempDir())
	got := mustCreate(t, svc, session.CreateOpts{Cwd: dir, Name: "work"})
	if *got.Bib != "01" || got.Name != "work" || got.Agent != "shell" || !got.Managed || got.Cwd != dir || got.Status != "idle" {
		t.Fatalf("Create = %+v, want managed shell session 01 named work in %s", got, dir)
	}
	if f.labels["w1"] != "work" {
		t.Fatalf("workspace label = %q, want the session name", f.labels["w1"])
	}
	if env := f.envs["w1"]; env["TOGO_SESSION"] != got.ID || env["TOGO_BIB"] != "01" {
		t.Fatalf("workspace env = %v, want TOGO_SESSION and TOGO_BIB", env)
	}
	if len(f.started) != 0 {
		t.Fatalf("started agents %+v for the plain shell", f.started)
	}
	list := mustList(t, svc)
	if len(list) != 1 || list[0].ID != got.ID || list[0].Name != "work" || !list[0].CreatedAt.Equal(fixedNow) {
		t.Fatalf("List = %+v, want just %s", list, got.ID)
	}
	if second := mustCreate(t, svc, session.CreateOpts{}); *second.Bib != "02" {
		t.Fatalf("second session bib = %s, want 02", *second.Bib)
	}
}

func TestHerdrCreateStartsTheAgentWithHerdr(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, fakeBin(t, "claude"))
	got := mustCreate(t, svc, session.CreateOpts{Agent: "claude", Args: []string{"--model", "x"}})
	want := []herdr.StartOpts{{Name: got.ID, Kind: "claude", PaneID: "w1:p1", Args: []string{"--model", "x"}, Timeout: f.started[0].Timeout}}
	if !reflect.DeepEqual(f.started, want) || want[0].Timeout <= 0 {
		t.Fatalf("started %+v, want %+v with a timeout", f.started, want)
	}
	if got.Agent != "claude" || len(got.Command) == 0 {
		t.Fatalf("Create = %+v, want the claude session with its command", got)
	}
}

func TestHerdrAgentHeldAtAStartupDialogIsStillCreated(t *testing.T) {
	f := newFakeHerdr()
	f.startErr = herdr.ErrAgentNotReady
	svc, _ := herdrService(t, f, fakeBin(t, "claude"))
	got := mustCreate(t, svc, session.CreateOpts{Agent: "claude"})
	if list := mustList(t, svc); len(list) != 1 || list[0].ID != got.ID {
		t.Fatalf("List = %+v, want the session waiting at its dialog", list)
	}
}

func TestHerdrFailedAgentStartLeavesNothingBehind(t *testing.T) {
	f := newFakeHerdr()
	f.startErr = &herdr.APIError{Code: "timeout", Message: "timed out waiting for agent startup"}
	svc, _ := herdrService(t, f, fakeBin(t, "claude"))
	if _, err := svc.Create(context.Background(), session.CreateOpts{Agent: "claude", Cwd: t.TempDir()}); err == nil {
		t.Fatal("Create succeeded with the agent never starting")
	}
	if len(f.labels) != 0 || !slices.Equal(f.closed, []string{"w1"}) {
		t.Fatalf("workspaces %v, closed %v; want w1 closed again", f.labels, f.closed)
	}
	if list := mustList(t, svc); len(list) != 0 {
		t.Fatalf("List = %+v, want none", list)
	}
}

func TestHerdrRefusesAnAgentHerdrCannotStart(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, fakeBin(t, "aider"))
	_, err := svc.Create(context.Background(), session.CreateOpts{Agent: "aider", Cwd: t.TempDir()})
	if got := reason(t, err); got != wire.ReasonProgram {
		t.Fatalf("Create aider reason = %q, want %q", got, wire.ReasonProgram)
	}
	if f.n != 0 {
		t.Fatalf("%d workspaces created for a refused agent", f.n)
	}
}

func TestHerdrRenameMuteAndExitAreStored(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, "")
	ctx := context.Background()
	s := mustCreate(t, svc, session.CreateOpts{})
	if got, err := svc.Rename(ctx, s.ID, "renamed"); err != nil || got.Name != "renamed" {
		t.Fatalf("Rename = %+v, %v", got, err)
	}
	if f.labels["w1"] != "renamed" {
		t.Fatalf("workspace label = %q, want renamed", f.labels["w1"])
	}
	if got, err := svc.SetMuted(ctx, s.ID, true); err != nil || !got.Muted {
		t.Fatalf("SetMuted = %+v, %v", got, err)
	}
	code := 3
	saved := &session.SavedExit{Status: wire.StatusFailed, Exit: wire.SessionExit{Reason: wire.ExitReasonExit, Code: &code}}
	if err := svc.SaveExit(ctx, s.ID, saved); err != nil {
		t.Fatalf("SaveExit: %v", err)
	}
	list := mustList(t, svc)
	if len(list) != 1 || !list[0].Muted || list[0].SavedExit == nil || *list[0].SavedExit.Exit.Code != 3 {
		t.Fatalf("List = %+v, want muted with the saved exit", list)
	}
	if err := svc.SaveExit(ctx, s.ID, nil); err != nil {
		t.Fatalf("SaveExit(nil): %v", err)
	}
	if list := mustList(t, svc); list[0].SavedExit != nil {
		t.Fatalf("SavedExit = %+v after clearing it", list[0].SavedExit)
	}
}

func TestHerdrKillClosesTheWorkspace(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, "")
	ctx := context.Background()
	s := mustCreate(t, svc, session.CreateOpts{})
	if err := svc.Kill(ctx, s.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if len(f.labels) != 0 {
		t.Fatalf("workspaces %v left after Kill", f.labels)
	}
	if err := svc.Kill(ctx, s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("second Kill = %v, want ErrNotFound", err)
	}
}

func TestHerdrWorkspaceClosedInHerdrEndsTheSession(t *testing.T) {
	f := newFakeHerdr()
	svc, b := herdrService(t, f, "")
	s := mustCreate(t, svc, session.CreateOpts{})
	f.closeInHerdr("w1")
	if list := mustList(t, svc); len(list) != 0 {
		t.Fatalf("List = %+v after the workspace closed", list)
	}
	raw, err := os.ReadFile(b.Store)
	if err != nil || strings.Contains(string(raw), s.ID) {
		t.Fatalf("store %q, %v; want the closed session dropped", raw, err)
	}
	if _, err := svc.AttachCommand(context.Background(), s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("AttachCommand = %v, want ErrNotFound", err)
	}
}

func TestHerdrServerDownKeepsTheSessions(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, "")
	s := mustCreate(t, svc, session.CreateOpts{})
	f.down = true
	if _, err := svc.List(context.Background()); err == nil {
		t.Fatal("List succeeded with the herdr server down")
	}
	var unavailable *session.HerdrUnavailableError
	if _, err := svc.List(context.Background()); !errors.As(err, &unavailable) {
		t.Fatalf("List = %v, want *HerdrUnavailableError", err)
	}
	f.down = false
	if list := mustList(t, svc); len(list) != 1 || list[0].ID != s.ID {
		t.Fatalf("List = %+v once the server is back, want %s", list, s.ID)
	}
}

func TestHerdrAttachAndCaptureUseTheRootPane(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, "")
	ctx := context.Background()
	s := mustCreate(t, svc, session.CreateOpts{})
	for _, attach := range []func(context.Context, string) ([]string, error){svc.AttachCommand, svc.PhoneAttachCommand} {
		argv, err := attach(ctx, s.ID)
		if err != nil || !slices.Equal(argv, []string{"herdr", "terminal", "attach", "term_1", "--session", "fake"}) {
			t.Fatalf("attach = %v, %v; want the terminal attach argv", argv, err)
		}
	}
	text, truncated, err := svc.Capture(ctx, s.ID, 200, true)
	if err != nil || truncated || text != "screen of w1:p1 from recent-unwrapped, 200 lines" {
		t.Fatalf("Capture = %q, %v, %v", text, truncated, err)
	}
}

func TestHerdrSendTextPromptsTheAgent(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, fakeBin(t, "claude"))
	ctx := context.Background()
	s := mustCreate(t, svc, session.CreateOpts{Agent: "claude"})
	if err := svc.SendText(ctx, s.ID, "fix it\nplease", true, "", nil); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if !slices.Equal(f.prompted, []string{"w1:p1 fix it\nplease"}) {
		t.Fatalf("prompted %q, want one prompt to the pane", f.prompted)
	}
	if err := svc.SendText(ctx, s.ID, "-v please", true, "", nil); err != nil {
		t.Fatalf("SendText with a leading dash: %v", err)
	}
	if !slices.Equal(f.typed, []string{"-v please"}) || !slices.Equal(f.keys, []string{"enter"}) {
		t.Fatalf("typed %q and pressed %q, want the text typed and Enter", f.typed, f.keys)
	}
	if err := svc.SendText(ctx, s.ID, "two\nlines", false, "", nil); reason(t, err) != wire.ReasonText {
		t.Fatalf("SendText of two lines without submit = %v, want a text rejection", err)
	}
}

func TestHerdrSendTextRefusals(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, fakeBin(t, "claude"))
	ctx := context.Background()
	s := mustCreate(t, svc, session.CreateOpts{Agent: "claude"})
	refused := func(want string) {
		t.Helper()
		var r *session.ReplyRefusedError
		if err := svc.SendText(ctx, s.ID, "hi", true, "", nil); !errors.As(err, &r) || r.Reason != want {
			t.Fatalf("SendText = %v, want refused with %s", err, want)
		}
	}
	f.promptErr = herdr.ErrAgentBlocked
	refused(wire.ConflictNeedsYou)
	f.promptErr = nil
	f.exitAgent("w1:p1")
	refused(wire.ConflictEnded)
	if len(f.prompted) != 1 {
		t.Fatalf("prompted %q, want only the blocked attempt", f.prompted)
	}
}

func TestHerdrRelaunchStartsTheAgentOnlyOnceItHasExited(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, fakeBin(t, "claude"))
	ctx := context.Background()
	s := mustCreate(t, svc, session.CreateOpts{Agent: "claude", Cwd: realpath(t, t.TempDir())})
	if _, err := svc.Relaunch(ctx, s.ID); !errors.Is(err, session.ErrRunning) {
		t.Fatalf("Relaunch while running = %v, want ErrRunning", err)
	}
	if err := svc.RelaunchWith(ctx, s.ID, []string{"claude", "--resume"}); !errors.Is(err, session.ErrRunning) {
		t.Fatalf("RelaunchWith while running = %v, want ErrRunning", err)
	}
	f.exitAgent("w1:p1")
	if gone, err := svc.AgentGone(ctx, s.ID); err != nil || !gone {
		t.Fatalf("AgentGone = %v, %v; want gone", gone, err)
	}
	if _, err := svc.Relaunch(ctx, s.ID); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if len(f.started) != 2 || f.started[1].Name != s.ID || f.started[1].PaneID != "w1:p1" {
		t.Fatalf("started %+v, want the agent started again in w1:p1", f.started)
	}
}

func TestHerdrRestorableDropsSessionsClosedWhileTheServerRuns(t *testing.T) {
	f := newFakeHerdr()
	svc, _ := herdrService(t, f, "")
	ctx := context.Background()
	mustCreate(t, svc, session.CreateOpts{})
	f.closeInHerdr("w1")
	got, err := svc.Restorable(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("Restorable = %+v, %v; want none for a session closed at the desk", got, err)
	}
}

func TestNewServiceChoosesTheBackend(t *testing.T) {
	ctx := context.Background()
	t.Setenv("TOGO_BACKEND", "herdr")
	t.Setenv("TOGO_HERDR_SESSION", "")
	home := t.TempDir()
	svc, err := session.NewService(ctx, home)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	b, ok := svc.Backend.(*session.HerdrBackend)
	if !ok {
		t.Fatalf("Backend = %T, want *HerdrBackend", svc.Backend)
	}
	if c := b.Client.(*herdr.Client); c.Session != "default" || !filepath.IsAbs(c.Bin) && c.Bin != "herdr" {
		t.Fatalf("herdr client = %+v, want the default session", c)
	}
	if b.Store != filepath.Join(home, "herdr-sessions.json") {
		t.Fatalf("Store = %s", b.Store)
	}
	t.Setenv("TOGO_HERDR_SESSION", "lab")
	if svc, err = session.NewService(ctx, home); err != nil || svc.Backend.(*session.HerdrBackend).Client.(*herdr.Client).Session != "lab" {
		t.Fatalf("NewService with TOGO_HERDR_SESSION=lab: %v", err)
	}
	t.Setenv("TOGO_BACKEND", "tmux")
	if svc, err = session.NewService(ctx, home); err != nil || svc.Backend != nil {
		t.Fatalf("NewService with tmux = %v, %v; want no Backend", svc, err)
	}
	t.Setenv("TOGO_BACKEND", "screen")
	if _, err := session.NewService(ctx, home); err == nil {
		t.Fatal("NewService accepted an unknown backend")
	}
}
