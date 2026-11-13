package herdr_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/gettogo/togo/helper/internal/herdr"
)

func TestListPanesParsesPanesAndNullCwd(t *testing.T) {
	c, f := newFake(t)
	f.reply(panesJSON, "", 0)
	got, err := c.ListPanes(context.Background(), "w1")
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "pane", "list", "--workspace", "w1", "--session", "togo-test")
	want := []herdr.Pane{{
		ID: "w1:p1", WorkspaceID: "w1", TabID: "w1:t1", TerminalID: "term_1", Focused: true,
		Cwd: "/tmp/a", Agent: "claude", AgentStatus: "idle", Revision: 7,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListPanes = %+v, want %+v", got, want)
	}
}

func TestListPanesWithoutWorkspaceListsAll(t *testing.T) {
	c, f := newFake(t)
	f.reply(panesJSON, "", 0)
	if _, err := c.ListPanes(context.Background(), ""); err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "pane", "list", "--session", "togo-test")
}

func TestSendTextPassesTextAsOneArgument(t *testing.T) {
	c, f := newFake(t)
	f.reply("", "", 0)
	// Herdr types flag-like text literally; it must arrive untouched.
	const text = "--help; $(rm -rf ~) 'q'"
	if err := c.SendText(context.Background(), "w1:p1", text); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "pane", "send-text", "w1:p1", text, "--session", "togo-test")
}

func TestSendTextRefusesTextThatCouldRetargetTheSession(t *testing.T) {
	for _, text := range []string{"--session", "--session=default"} {
		c, f := newFake(t)
		if err := c.SendText(context.Background(), "w1:p1", text); err == nil {
			t.Errorf("SendText(%q) succeeded, want an error", text)
		}
		if n := len(f.calls()); n != 0 {
			t.Errorf("SendText(%q): herdr ran %d times, want 0", text, n)
		}
	}
}

func TestEmptySendsRunNothing(t *testing.T) {
	c, f := newFake(t)
	if err := c.SendText(context.Background(), "w1:p1", ""); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if err := c.SendKeys(context.Background(), "w1:p1"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("herdr ran %d times, want 0", n)
	}
}

func TestSendKeysInOrder(t *testing.T) {
	c, f := newFake(t)
	f.reply("", "", 0)
	if err := c.SendKeys(context.Background(), "w1:p1", "ctrl+u", "esc", "enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "pane", "send-keys", "w1:p1", "ctrl+u", "esc", "enter", "--session", "togo-test")
}

func TestPaneIDThatLooksLikeAFlagRunsNothing(t *testing.T) {
	c, f := newFake(t)
	ctx := context.Background()
	if err := c.SendText(ctx, "--current", "x"); err == nil {
		t.Error("SendText to pane --current succeeded, want an error")
	}
	if err := c.SendKeys(ctx, "", "enter"); err == nil {
		t.Error("SendKeys to an empty pane id succeeded, want an error")
	}
	if _, err := c.ReadPane(ctx, "-w1", herdr.ReadOpts{}); err == nil {
		t.Error("ReadPane of pane -w1 succeeded, want an error")
	}
	if err := c.CloseWorkspace(ctx, ""); err == nil {
		t.Error("CloseWorkspace of an empty id succeeded, want an error")
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("herdr ran %d times, want 0", n)
	}
}

func TestReadPaneReturnsTextVerbatim(t *testing.T) {
	c, f := newFake(t)
	const screen = "$ echo hi\nhi\n$ "
	f.reply(screen, "", 0)
	got, err := c.ReadPane(context.Background(), "w1:p1", herdr.ReadOpts{})
	if err != nil {
		t.Fatalf("ReadPane: %v", err)
	}
	if got != screen {
		t.Fatalf("ReadPane = %q, want %q", got, screen)
	}
	wantArgs(t, f.onlyArgs(), "pane", "read", "w1:p1", "--source", "recent", "--session", "togo-test")
}

func TestReadPaneOptions(t *testing.T) {
	c, f := newFake(t)
	f.reply("", "", 0)
	o := herdr.ReadOpts{Source: herdr.SourceRecentUnwrapped, Lines: 120, ANSI: true}
	if _, err := c.ReadPane(context.Background(), "w1:p1", o); err != nil {
		t.Fatalf("ReadPane: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "pane", "read", "w1:p1", "--source", "recent-unwrapped",
		"--lines", "120", "--format", "ansi", "--session", "togo-test")
}

func TestAttachArgvNamesTheSession(t *testing.T) {
	c := herdr.New("togo")
	wantArgs(t, c.AttachArgv("term_1"), "herdr", "terminal", "attach", "term_1", "--session", "togo")
}
