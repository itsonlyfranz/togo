package herdr_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gettogo/togo/helper/internal/herdr"
)

// The fixtures below are real herdr 0.9.5 output, trimmed of fields the
// client does not read.
const (
	createdJSON = `{"id":"cli:workspace:create","result":{"root_pane":{"agent_status":"unknown","cwd":"/tmp/cwd x","focused":true,"foreground_cwd":"/tmp/cwd x","pane_id":"w1:p1","revision":0,"tab_id":"w1:t1","terminal_id":"term_66024c81cb3d51","workspace_id":"w1"},"tab":{"agent_status":"unknown","focused":true,"label":"1","number":1,"pane_count":1,"tab_id":"w1:t1","workspace_id":"w1"},"type":"workspace_created","workspace":{"active_tab_id":"w1:t1","agent_status":"unknown","focused":true,"label":"my \"label\"; $(x)","number":1,"pane_count":1,"tab_count":1,"workspace_id":"w1"}}}`
	listJSON    = `{"id":"cli:workspace:list","result":{"type":"workspace_list","workspaces":[{"active_tab_id":"w2:t1","agent_status":"working","focused":true,"label":"cwd x","number":1,"pane_count":1,"tab_count":1,"workspace_id":"w2","tokens":{"togo_id":"s_1"}}]}}`
	emptyJSON   = `{"id":"cli:workspace:list","result":{"type":"workspace_list","workspaces":[]}}`
	panesJSON   = `{"id":"cli:pane:list","result":{"panes":[{"agent":"claude","agent_status":"idle","cwd":"/tmp/a","focused":true,"foreground_cwd":null,"pane_id":"w1:p1","revision":7,"tab_id":"w1:t1","terminal_id":"term_1","workspace_id":"w1"}],"type":"pane_list"}}`
	renamedJSON = `{"id":"cli:workspace:rename","result":{"type":"workspace_info","workspace":{"active_tab_id":"w1:t1","agent_status":"unknown","focused":true,"label":"-dash","number":1,"pane_count":1,"tab_count":1,"workspace_id":"w1"}}}`
	okJSON      = `{"id":"cli:workspace:close","result":{"type":"ok"}}`
	notRunning  = `{"id":"cli:workspace:list","error":{"code":"server_not_running","message":"no herdr server is running at /x/herdr.sock"}}`
	wsNotFound  = `{"error":{"code":"workspace_not_found","message":"workspace w9 not found"},"id":"cli:workspace:close"}`
	paneMissing = `{"error":{"code":"pane_not_found","message":"pane w9:p9 not found"},"id":"cli:request"}`
	badKey      = `{"error":{"code":"invalid_key","message":"unsupported key bogus-key"},"id":"cli:request"}`
	statusUp    = `{"client":{"version":"0.9.5"},"server":{"status":"running","running":true,"version":"0.9.5","protocol":22,"compatible":true}}`
	statusDown  = `{"client":{"version":"0.9.5"},"server":{"status":"not_running","running":false,"version":null,"protocol":null,"compatible":null}}`
)

func wantArgs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv =\n  %q\nwant\n  %q", got, want)
	}
}

func TestEveryCallNamesTheSessionAndDropsInheritedPaneContext(t *testing.T) {
	c, f := newFake(t)
	for _, k := range []string{"HERDR_SESSION", "HERDR_SOCKET_PATH", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID", "HERDR_PANE_ID"} {
		t.Setenv(k, "from-the-enclosing-pane")
	}
	f.reply(emptyJSON, "", 0)
	if _, err := c.ListWorkspaces(context.Background()); err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	call := f.calls()[0]
	wantArgs(t, call.Args, "workspace", "list", "--session", "togo-test")
	if len(call.Env) != 0 {
		t.Fatalf("herdr saw inherited pane context %v, want none", call.Env)
	}
}

func TestInvalidSessionNameRunsNothing(t *testing.T) {
	for _, name := range []string{"", "-x"} {
		c, f := newFake(t)
		c.Session = name
		if _, err := c.ListWorkspaces(context.Background()); err == nil {
			t.Errorf("session %q: ListWorkspaces succeeded, want an error", name)
		}
		if n := len(f.calls()); n != 0 {
			t.Errorf("session %q: herdr ran %d times, want 0", name, n)
		}
	}
}

func TestCreateWorkspacePassesOptionsAsArgv(t *testing.T) {
	c, f := newFake(t)
	f.reply(createdJSON, "", 0)
	const label = `my "label"; $(x)`
	got, err := c.CreateWorkspace(context.Background(), herdr.CreateOpts{
		Label: label,
		Dir:   "/tmp/cwd x",
		Env:   map[string]string{"TOGO_Q": "a b", "TOGO_SID": "abc=1"},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "workspace", "create", "--label", label, "--cwd", "/tmp/cwd x",
		"--env", "TOGO_Q=a b", "--env", "TOGO_SID=abc=1", "--no-focus", "--session", "togo-test")
	if got.Workspace.ID != "w1" || got.Workspace.Label != label || got.TabID != "w1:t1" {
		t.Errorf("Created = %+v, want workspace w1 labelled %q in tab w1:t1", got, label)
	}
	if got.RootPane.ID != "w1:p1" || got.RootPane.TerminalID != "term_66024c81cb3d51" || got.RootPane.Cwd != "/tmp/cwd x" {
		t.Errorf("RootPane = %+v, want pane w1:p1 on term_66024c81cb3d51 in /tmp/cwd x", got.RootPane)
	}
}

func TestCreateWorkspaceDefaultsLeaveHerdrDefaultsAndFocusIsOptIn(t *testing.T) {
	c, f := newFake(t)
	f.reply(createdJSON, "", 0)
	if _, err := c.CreateWorkspace(context.Background(), herdr.CreateOpts{Focus: true}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "workspace", "create", "--focus", "--session", "togo-test")
}

func TestCreateWorkspaceRejectsAnEnvKeyHoldingEquals(t *testing.T) {
	c, f := newFake(t)
	_, err := c.CreateWorkspace(context.Background(), herdr.CreateOpts{Env: map[string]string{"A=B": "c"}})
	if err == nil {
		t.Fatal("CreateWorkspace succeeded, want an error for key A=B")
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("herdr ran %d times, want 0", n)
	}
}

func TestListWorkspacesParsesWorkspaces(t *testing.T) {
	c, f := newFake(t)
	f.reply(listJSON, "", 0)
	got, err := c.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	want := []herdr.Workspace{{
		ID: "w2", Number: 1, Label: "cwd x", Focused: true, ActiveTabID: "w2:t1",
		TabCount: 1, PaneCount: 1, AgentStatus: "working", Tokens: map[string]string{"togo_id": "s_1"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListWorkspaces = %+v, want %+v", got, want)
	}
}

func TestListWorkspacesEmptyIsNonNil(t *testing.T) {
	c, f := newFake(t)
	f.reply(emptyJSON, "", 0)
	got, err := c.ListWorkspaces(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("ListWorkspaces = %v, %v; want an empty non-nil list", got, err)
	}
}

func TestListsWithNoServerReturnNil(t *testing.T) {
	c, f := newFake(t)
	f.reply("", notRunning, 1)
	ws, err := c.ListWorkspaces(context.Background())
	if ws != nil || err != nil {
		t.Fatalf("ListWorkspaces = %v, %v; want nil, nil with no server", ws, err)
	}
	panes, err := c.ListPanes(context.Background(), "")
	if panes != nil || err != nil {
		t.Fatalf("ListPanes = %v, %v; want nil, nil with no server", panes, err)
	}
}

func TestRenameWorkspaceKeepsALeadingDashLabel(t *testing.T) {
	c, f := newFake(t)
	f.reply(renamedJSON, "", 0)
	got, err := c.RenameWorkspace(context.Background(), "w1", "-dash")
	if err != nil {
		t.Fatalf("RenameWorkspace: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "workspace", "rename", "w1", "-dash", "--session", "togo-test")
	if got.Label != "-dash" {
		t.Fatalf("Label = %q, want %q", got.Label, "-dash")
	}
}

func TestRenameWorkspaceRejectsEmptyLabel(t *testing.T) {
	c, f := newFake(t)
	if _, err := c.RenameWorkspace(context.Background(), "w1", ""); err == nil {
		t.Fatal("RenameWorkspace with an empty label succeeded, want an error")
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("herdr ran %d times, want 0", n)
	}
}

func TestCloseWorkspace(t *testing.T) {
	c, f := newFake(t)
	f.reply(okJSON, "", 0)
	if err := c.CloseWorkspace(context.Background(), "w1"); err != nil {
		t.Fatalf("CloseWorkspace: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "workspace", "close", "w1", "--session", "togo-test")
}

func TestErrorsCarryHerdrCodes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		stderr string
		call   func(*herdr.Client) error
		is     error
		code   string
	}{
		{"close missing workspace", wsNotFound, func(c *herdr.Client) error { return c.CloseWorkspace(ctx, "w9") }, herdr.ErrNotFound, "workspace_not_found"},
		{"send to missing pane", paneMissing, func(c *herdr.Client) error { return c.SendText(ctx, "w9:p9", "x") }, herdr.ErrNotFound, "pane_not_found"},
		{"close with no server", notRunning, func(c *herdr.Client) error { return c.CloseWorkspace(ctx, "w1") }, herdr.ErrServerNotRunning, "server_not_running"},
		{"unknown key", badKey, func(c *herdr.Client) error { return c.SendKeys(ctx, "w1:p1", "bogus-key") }, nil, "invalid_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFake(t)
			f.reply("", tc.stderr, 1)
			err := tc.call(c)
			var apiErr *herdr.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != tc.code {
				t.Fatalf("error = %v, want an APIError with code %s", err, tc.code)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("error = %v, want errors.Is %v", err, tc.is)
			}
			if tc.is == nil && (errors.Is(err, herdr.ErrNotFound) || errors.Is(err, herdr.ErrServerNotRunning)) {
				t.Fatalf("error = %v matched a sentinel it should not", err)
			}
		})
	}
}

func TestUsageErrorKeepsStderrText(t *testing.T) {
	c, f := newFake(t)
	f.reply("", "unknown option: --label=-x\n", 2)
	err := c.CloseWorkspace(context.Background(), "w1")
	var apiErr *herdr.APIError
	if err == nil || errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want a plain error", err)
	}
	if want := "herdr: unknown option: --label=-x"; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error = %q, want it to start with %q", err, want)
	}
}

func TestNotInstalled(t *testing.T) {
	c := &herdr.Client{Bin: filepath.Join(t.TempDir(), "no-herdr"), Session: "togo-test"}
	if _, err := c.ListWorkspaces(context.Background()); !errors.Is(err, herdr.ErrNotInstalled) {
		t.Fatalf("error = %v, want ErrNotInstalled", err)
	}
	c = &herdr.Client{Bin: "togo-no-such-herdr-binary", Session: "togo-test"}
	if _, err := c.Status(context.Background()); !errors.Is(err, herdr.ErrNotInstalled) {
		t.Fatalf("error = %v, want ErrNotInstalled", err)
	}
}

func TestStatus(t *testing.T) {
	c, f := newFake(t)
	f.reply(statusUp, "", 0)
	got, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if want := (herdr.ServerStatus{Running: true, Version: "0.9.5", Protocol: 22, Compatible: true}); got != want {
		t.Fatalf("Status = %+v, want %+v", got, want)
	}
	wantArgs(t, f.onlyArgs(), "status", "--json", "--session", "togo-test")

	f.reply(statusDown, "", 0)
	got, err = c.Status(context.Background())
	if err != nil || got != (herdr.ServerStatus{}) {
		t.Fatalf("Status = %+v, %v; want a zero status for a stopped server", got, err)
	}
}
