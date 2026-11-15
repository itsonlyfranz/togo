package herdr_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gettogo/togo/helper/internal/herdr"
)

// processInfoJSON is real herdr 0.9.5 output for a pane running claude.
// The reported name is a version string, so callers go by pid.
const processInfoJSON = `{"id":"cli:pane:process_info","result":{"process_info":{"foreground_process_group_id":27316,` +
	`"foreground_processes":[{"argv":["claude"],"argv0":"claude","cmdline":"claude","cwd":"/tmp/a","name":"2.1.325","pid":27316}],` +
	`"pane_id":"w1:p1","shell_pid":26572},"type":"pane_process_info"}}`

func TestStartAgentPutsTheSessionBeforeTheAgentArguments(t *testing.T) {
	c, f := newFake(t)
	f.reply(`{"id":"cli:agent:start","result":{"type":"ok"}}`, "", 0)
	err := c.StartAgent(context.Background(), herdr.StartOpts{
		Name: "s_abcdefghij", Kind: "claude", PaneID: "w1:p1",
		Args: []string{"--session", "x", "--model=y"}, Timeout: 1500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	// An agent argument such as "--session" must stay the agent's, after
	// "--", and never become Herdr's own session option.
	wantArgs(t, f.onlyArgs(), "agent", "start", "s_abcdefghij", "--kind", "claude", "--pane", "w1:p1",
		"--timeout", "1500", "--session", "togo-test", "--", "--session", "x", "--model=y")
}

func TestStartAgentWithNoArgumentsStillEndsHerdrOptions(t *testing.T) {
	c, f := newFake(t)
	if err := c.StartAgent(context.Background(), herdr.StartOpts{Name: "a", Kind: "codex", PaneID: "w1:p1"}); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "agent", "start", "a", "--kind", "codex", "--pane", "w1:p1", "--session", "togo-test", "--")
}

func TestStartAgentHeldAtAStartupDialogIsNotReady(t *testing.T) {
	c, f := newFake(t)
	f.reply("", `{"error":{"code":"agent_not_ready","message":"agent a is blocked during startup and is not ready for prompts"},"id":"cli:agent:start"}`, 1)
	err := c.StartAgent(context.Background(), herdr.StartOpts{Name: "a", Kind: "claude", PaneID: "w1:p1"})
	if !errors.Is(err, herdr.ErrAgentNotReady) {
		t.Fatalf("StartAgent = %v, want ErrAgentNotReady", err)
	}
}

func TestStartAgentRejectsBadNamesAndIdsBeforeRunning(t *testing.T) {
	for _, o := range []herdr.StartOpts{
		{Name: "S_upper", Kind: "claude", PaneID: "w1:p1"},
		{Name: "", Kind: "claude", PaneID: "w1:p1"},
		{Name: "a", Kind: "--session", PaneID: "w1:p1"},
		{Name: "a", Kind: "claude", PaneID: "-w1"},
	} {
		c, f := newFake(t)
		if err := c.StartAgent(context.Background(), o); err == nil {
			t.Errorf("StartAgent(%+v) succeeded, want an error", o)
		}
		if n := len(f.calls()); n != 0 {
			t.Errorf("StartAgent(%+v): herdr ran %d times, want 0", o, n)
		}
	}
}

func TestPromptAgentPassesTextAsOneArgument(t *testing.T) {
	c, f := newFake(t)
	const text = "fix it; $(rm -rf ~)\nthen 'test'"
	if err := c.PromptAgent(context.Background(), "w1:p1", text); err != nil {
		t.Fatalf("PromptAgent: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "agent", "prompt", "w1:p1", text, "--session", "togo-test")
}

func TestPromptAgentMapsBlockedAndMissingAgents(t *testing.T) {
	for code, want := range map[string]error{"agent_blocked": herdr.ErrAgentBlocked, "agent_not_found": herdr.ErrNoAgent} {
		c, f := newFake(t)
		f.reply("", `{"error":{"code":"`+code+`","message":"m"},"id":"cli:agent:prompt"}`, 1)
		if err := c.PromptAgent(context.Background(), "w1:p1", "hi"); !errors.Is(err, want) {
			t.Errorf("PromptAgent with %s = %v, want %v", code, err, want)
		}
	}
}

func TestPromptAgentRefusesTextHerdrWouldReadAsAnOption(t *testing.T) {
	// herdr 0.9.5 answers "unknown option: -h" and has no "--" for it.
	for _, text := range []string{"-h", "--wait", ""} {
		c, f := newFake(t)
		if err := c.PromptAgent(context.Background(), "w1:p1", text); err == nil {
			t.Errorf("PromptAgent(%q) succeeded, want an error", text)
		}
		if n := len(f.calls()); n != 0 {
			t.Errorf("PromptAgent(%q): herdr ran %d times, want 0", text, n)
		}
	}
}

func TestPaneProcessesParsesTheForegroundJob(t *testing.T) {
	c, f := newFake(t)
	f.reply(processInfoJSON, "", 0)
	info, err := c.PaneProcesses(context.Background(), "w1:p1")
	if err != nil {
		t.Fatalf("PaneProcesses: %v", err)
	}
	wantArgs(t, f.onlyArgs(), "pane", "process-info", "--pane", "w1:p1", "--session", "togo-test")
	if info.ShellPID != 26572 || len(info.Foreground) != 1 || info.Foreground[0].Argv0 != "claude" || info.Foreground[0].PID != 27316 {
		t.Fatalf("PaneProcesses = %+v, want claude 27316 over shell 26572", info)
	}
	if info.ShellInForeground() {
		t.Fatal("ShellInForeground = true with claude in the foreground")
	}
}

func TestShellInForeground(t *testing.T) {
	for _, tc := range []struct {
		name string
		info herdr.ProcessInfo
		want bool
	}{
		{"shell alone", herdr.ProcessInfo{ShellPID: 5, Foreground: []herdr.Process{{PID: 5}}}, true},
		{"agent", herdr.ProcessInfo{ShellPID: 5, Foreground: []herdr.Process{{PID: 5}, {PID: 9}}}, false},
		{"unknown shell", herdr.ProcessInfo{Foreground: []herdr.Process{{PID: 5}}}, false},
	} {
		if got := tc.info.ShellInForeground(); got != tc.want {
			t.Errorf("%s: ShellInForeground = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPaneProcessesOfAMissingPaneIsNotFound(t *testing.T) {
	c, f := newFake(t)
	f.reply("", `{"error":{"code":"pane_not_found","message":"pane not found"},"id":"cli:pane:process_info"}`, 1)
	if _, err := c.PaneProcesses(context.Background(), "w9:p9"); !errors.Is(err, herdr.ErrNotFound) {
		t.Fatalf("PaneProcesses = %v, want ErrNotFound", err)
	}
}
