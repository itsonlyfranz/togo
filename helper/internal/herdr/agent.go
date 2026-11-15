package herdr

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrAgentNotReady is returned by StartAgent when the agent was detected
// but is held at a dialog during startup, such as a folder trust prompt.
// The agent is running and keeps its name; it is waiting for the user.
var ErrAgentNotReady = errors.New("herdr: agent blocked during startup")

// ErrAgentBlocked is returned by PromptAgent when the agent is waiting at
// an approval or question dialog. Nothing was typed.
var ErrAgentBlocked = errors.New("herdr: agent is waiting at a dialog")

// ErrNoAgent is returned when no live agent has the given name.
var ErrNoAgent = errors.New("herdr: no such agent")

// agentName is the form Herdr accepts for an agent name.
var agentName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// StartOpts configures StartAgent.
type StartOpts struct {
	// Name is the agent's unique live name, matching [a-z][a-z0-9_-]{0,31}.
	Name string
	// Kind is the Herdr agent kind, which also picks the executable, such
	// as "claude" or "codex".
	Kind string
	// PaneID is the pane to start in. Its shell must be at its prompt.
	PaneID string
	// Args are passed to the agent after "--", never read by Herdr.
	Args []string
	// Timeout bounds the wait for the agent to be ready; zero leaves
	// Herdr's default of 30 seconds.
	Timeout time.Duration
}

// StartAgent starts a supported agent in a pane at its shell prompt and
// waits until Herdr detects it ready for input. An agent held at a dialog
// during startup is ErrAgentNotReady, though it is running.
func (c *Client) StartAgent(ctx context.Context, o StartOpts) error {
	if !agentName.MatchString(o.Name) {
		return fmt.Errorf("herdr: invalid agent name %q", o.Name)
	}
	if err := checkArg("agent kind", o.Kind, true); err != nil {
		return err
	}
	if err := checkArg("pane id", o.PaneID, true); err != nil {
		return err
	}
	args := []string{"agent", "start", o.Name, "--kind", o.Kind, "--pane", o.PaneID}
	if o.Timeout > 0 {
		args = append(args, "--timeout", strconv.FormatInt(o.Timeout.Milliseconds(), 10))
	}
	// The agent's arguments follow "--", so Herdr never reads them as its
	// own options.
	_, err := c.runPassing(ctx, args, append([]string{}, o.Args...))
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "agent_not_ready" {
		return fmt.Errorf("%w: %s", ErrAgentNotReady, apiErr.Message)
	}
	return err
}

// PromptAgent submits text to an agent, named by its live name or by the
// pane that hosts it, as one ordered paste and Enter, honoring the pane's
// bracketed paste mode. It returns once both are written; it does not wait
// for the agent to act. An agent at a dialog is ErrAgentBlocked and a
// missing one ErrNoAgent; either way nothing is typed.
//
// Herdr has no end-of-options marker for the text, so text starting with
// "-" would be read as an option and is refused.
func (c *Client) PromptAgent(ctx context.Context, target, text string) error {
	if err := checkArg("agent", target, true); err != nil {
		return err
	}
	if err := checkText(text); err != nil {
		return err
	}
	_, err := c.run(ctx, "agent", "prompt", target, text)
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "agent_blocked":
			return fmt.Errorf("%w: %s", ErrAgentBlocked, apiErr.Message)
		case "agent_not_found":
			return fmt.Errorf("%w: %s", ErrNoAgent, target)
		}
	}
	return err
}

// checkText rejects text Herdr would parse as an option.
func checkText(text string) error {
	if text == "" {
		return errors.New("herdr: text must not be empty")
	}
	if strings.HasPrefix(text, "-") {
		return errors.New(`herdr: text starting with "-" would be read as an option`)
	}
	return nil
}

// Process is one process in a pane's foreground job.
type Process struct {
	PID int `json:"pid"`
	// Argv0 is the program name the process was started with; Name is
	// what the system reports, which for some programs is a version string.
	Argv0 string `json:"argv0"`
	Name  string `json:"name"`
}

// ProcessInfo is the shell and foreground job of a pane.
type ProcessInfo struct {
	ShellPID   int       `json:"shell_pid"`
	Foreground []Process `json:"foreground_processes"`
}

// ShellInForeground reports whether the pane's shell is the only process
// in its foreground, so no agent or command is running there.
func (p ProcessInfo) ShellInForeground() bool {
	if p.ShellPID < 1 {
		return false
	}
	for _, proc := range p.Foreground {
		if proc.PID != p.ShellPID {
			return false
		}
	}
	return true
}

// PaneProcesses returns the shell and foreground processes of the pane.
// A missing pane is ErrNotFound.
func (c *Client) PaneProcesses(ctx context.Context, paneID string) (ProcessInfo, error) {
	if err := checkArg("pane id", paneID, true); err != nil {
		return ProcessInfo{}, err
	}
	out, err := c.run(ctx, "pane", "process-info", "--pane", paneID)
	if err != nil {
		return ProcessInfo{}, err
	}
	var res struct {
		Info ProcessInfo `json:"process_info"`
	}
	if err := decodeResult("pane process-info", out, &res); err != nil {
		return ProcessInfo{}, err
	}
	return res.Info, nil
}
