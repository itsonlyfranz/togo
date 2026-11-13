package herdr

import (
	"context"
	"errors"
	"strconv"
)

// Pane is a Herdr pane. ID is workspace-qualified ("w1:p1") and never reused
// after the pane closes. TerminalID names the terminal for AttachArgv.
type Pane struct {
	ID          string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	TerminalID  string `json:"terminal_id"`
	Focused     bool   `json:"focused"`
	// Cwd is the shell's directory and ForegroundCwd that of the foreground
	// process; either is empty when Herdr does not know it.
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd"`
	Label         string `json:"label"`
	// Agent is the coding agent Herdr recognized in the pane, or "".
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	// Revision increases whenever the pane's screen changes.
	Revision uint64 `json:"revision"`
}

// Source selects which snapshot ReadPane returns.
type Source string

// Read sources, as Herdr names them on the command line.
const (
	// SourceVisible is the rendered viewport, with soft wraps as rows.
	SourceVisible Source = "visible"
	// SourceRecent is recent output, including scrollback, with soft wraps
	// as rows.
	SourceRecent Source = "recent"
	// SourceRecentUnwrapped is recent output with soft wraps rejoined, so a
	// long command or URL reads as one line.
	SourceRecentUnwrapped Source = "recent-unwrapped"
)

// ReadOpts configures ReadPane.
type ReadOpts struct {
	// Source is the snapshot to read; empty means SourceRecent.
	Source Source
	// Lines asks for that many rows of screen and scrollback; zero leaves
	// Herdr's default.
	Lines int
	// ANSI keeps colors and styling as escape sequences.
	ANSI bool
}

// ListPanes returns the panes of the workspace with the given id, or of
// every workspace when id is "". When no server is running it returns
// nil, nil.
func (c *Client) ListPanes(ctx context.Context, workspaceID string) ([]Pane, error) {
	args := []string{"pane", "list"}
	if workspaceID != "" {
		if err := checkArg("workspace id", workspaceID, true); err != nil {
			return nil, err
		}
		args = append(args, "--workspace", workspaceID)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		if errors.Is(err, ErrServerNotRunning) {
			return nil, nil
		}
		return nil, err
	}
	var res struct {
		Panes []Pane `json:"panes"`
	}
	if err := decodeResult("pane list", out, &res); err != nil {
		return nil, err
	}
	if res.Panes == nil {
		res.Panes = []Pane{}
	}
	return res.Panes, nil
}

// SendText types text into the pane literally, byte for byte. It is not a
// bracketed paste: a newline in text reaches the program as Enter, so a
// shell runs each line as it arrives. Empty text sends nothing.
func (c *Client) SendText(ctx context.Context, paneID, text string) error {
	if text == "" {
		return nil
	}
	if err := checkArg("pane id", paneID, true); err != nil {
		return err
	}
	if err := checkArg("text", text, false); err != nil {
		return err
	}
	_, err := c.run(ctx, "pane", "send-text", paneID, text)
	return err
}

// SendKeys presses keys in the pane, in order. Keys use Herdr's names, such
// as "enter", "esc", "tab" or "ctrl+c". Herdr validates every key before it
// writes any, so an unknown key sends nothing and fails with code
// "invalid_key". No keys sends nothing.
func (c *Client) SendKeys(ctx context.Context, paneID string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := checkArg("pane id", paneID, true); err != nil {
		return err
	}
	args := []string{"pane", "send-keys", paneID}
	for _, k := range keys {
		if err := checkArg("key", k, true); err != nil {
			return err
		}
		args = append(args, k)
	}
	_, err := c.run(ctx, args...)
	return err
}

// ReadPane returns a text snapshot of the pane's terminal. Herdr has no
// byte stream of pane output to subscribe to, so following a pane means
// reading again; Pane.Revision tells whether anything changed.
func (c *Client) ReadPane(ctx context.Context, paneID string, o ReadOpts) (string, error) {
	if err := checkArg("pane id", paneID, true); err != nil {
		return "", err
	}
	source := o.Source
	if source == "" {
		source = SourceRecent
	}
	args := []string{"pane", "read", paneID, "--source", string(source)}
	if o.Lines > 0 {
		args = append(args, "--lines", strconv.Itoa(o.Lines))
	}
	if o.ANSI {
		args = append(args, "--format", "ansi")
	}
	return c.run(ctx, args...)
}

// AttachArgv returns the argv that attaches a terminal directly to the
// terminal with the given id (Pane.TerminalID), as an interactive client
// that takes input and draws output. It performs no exec itself. Herdr's
// default detach key is ctrl+b q.
func (c *Client) AttachArgv(terminalID string) []string {
	return []string{c.Bin, "terminal", "attach", terminalID, "--session", c.Session}
}
