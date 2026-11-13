package herdr

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Workspace is a Herdr workspace. ID is the stable handle ("w1"); Number is
// its current position in the sidebar and changes as others close.
type Workspace struct {
	ID          string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	ActiveTabID string `json:"active_tab_id"`
	TabCount    int    `json:"tab_count"`
	PaneCount   int    `json:"pane_count"`
	// AgentStatus is Herdr's rollup for the workspace: idle, working,
	// blocked, done or unknown.
	AgentStatus string `json:"agent_status"`
	// Tokens is display metadata reported with workspace report-metadata;
	// nil when none is set.
	Tokens map[string]string `json:"tokens"`
}

// CreateOpts configures CreateWorkspace.
type CreateOpts struct {
	// Label names the workspace; empty leaves Herdr's default, the base name
	// of the starting directory.
	Label string
	// Dir is the root pane's starting directory; empty leaves Herdr's
	// default.
	Dir string
	// Env is set in the root pane's environment.
	Env map[string]string
	// Focus moves the user's view to the new workspace. Off by default so a
	// background create never steals focus.
	Focus bool
}

// Created is what CreateWorkspace returns: the workspace, its first tab and
// the root pane that runs its shell.
type Created struct {
	Workspace Workspace
	TabID     string
	RootPane  Pane
}

// ListWorkspaces returns every workspace in the session. When no server is
// running it returns nil, nil.
func (c *Client) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	out, err := c.run(ctx, "workspace", "list")
	if err != nil {
		if errors.Is(err, ErrServerNotRunning) {
			return nil, nil
		}
		return nil, err
	}
	var res struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	if err := decodeResult("workspace list", out, &res); err != nil {
		return nil, err
	}
	if res.Workspaces == nil {
		res.Workspaces = []Workspace{}
	}
	return res.Workspaces, nil
}

// CreateWorkspace creates a workspace whose root pane starts the user's
// shell. Herdr has no option to start a command in place of the shell.
func (c *Client) CreateWorkspace(ctx context.Context, o CreateOpts) (Created, error) {
	args := []string{"workspace", "create"}
	if o.Label != "" {
		if err := checkArg("label", o.Label, false); err != nil {
			return Created{}, err
		}
		args = append(args, "--label", o.Label)
	}
	if o.Dir != "" {
		if err := checkArg("directory", o.Dir, false); err != nil {
			return Created{}, err
		}
		args = append(args, "--cwd", o.Dir)
	}
	keys := make([]string, 0, len(o.Env))
	for k := range o.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Herdr splits KEY=VALUE at the first "=", so a key holding one
		// would set a different variable.
		if k == "" || strings.Contains(k, "=") {
			return Created{}, fmt.Errorf("herdr: invalid environment variable name %q", k)
		}
		args = append(args, "--env", k+"="+o.Env[k])
	}
	if o.Focus {
		args = append(args, "--focus")
	} else {
		args = append(args, "--no-focus")
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return Created{}, fmt.Errorf("herdr: create workspace: %w", err)
	}
	var res struct {
		Workspace Workspace `json:"workspace"`
		Tab       struct {
			ID string `json:"tab_id"`
		} `json:"tab"`
		RootPane Pane `json:"root_pane"`
	}
	if err := decodeResult("workspace create", out, &res); err != nil {
		return Created{}, err
	}
	return Created{Workspace: res.Workspace, TabID: res.Tab.ID, RootPane: res.RootPane}, nil
}

// RenameWorkspace sets the label of the workspace with the given id and
// returns the workspace as Herdr now reports it.
func (c *Client) RenameWorkspace(ctx context.Context, id, label string) (Workspace, error) {
	if err := checkArg("workspace id", id, true); err != nil {
		return Workspace{}, err
	}
	if label == "" {
		return Workspace{}, errors.New("herdr: a workspace label must not be empty")
	}
	if err := checkArg("label", label, false); err != nil {
		return Workspace{}, err
	}
	out, err := c.run(ctx, "workspace", "rename", id, label)
	if err != nil {
		return Workspace{}, err
	}
	var res struct {
		Workspace Workspace `json:"workspace"`
	}
	if err := decodeResult("workspace rename", out, &res); err != nil {
		return Workspace{}, err
	}
	return res.Workspace, nil
}

// CloseWorkspace closes the workspace with the given id and every pane in
// it, ending their processes. A missing workspace is ErrNotFound; with no
// server running it is ErrServerNotRunning.
func (c *Client) CloseWorkspace(ctx context.Context, id string) error {
	if err := checkArg("workspace id", id, true); err != nil {
		return err
	}
	_, err := c.run(ctx, "workspace", "close", id)
	return err
}
