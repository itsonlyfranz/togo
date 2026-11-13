// Package herdr is a thin driver for the Herdr command line tool, a terminal
// workspace manager for coding agents. Every call execs the binary directly
// with an argv slice; nothing is ever run through a shell.
//
// A Client is bound to one named Herdr session and passes "--session NAME"
// on every call, so it never falls through to whichever session the
// environment would select. In Herdr a session is a whole server; inside it,
// workspaces hold tabs and tabs hold panes. A togo session maps to one
// workspace, whose root pane is the terminal.
package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrNotInstalled is returned when the herdr binary cannot be found.
var ErrNotInstalled = errors.New("herdr: not installed")

// ErrServerNotRunning is returned when no Herdr server is running for the
// client's session. The List methods return nil, nil instead.
var ErrServerNotRunning = errors.New("herdr: server not running")

// ErrNotFound is returned when the targeted workspace or pane does not exist.
var ErrNotFound = errors.New("herdr: no such workspace or pane")

// APIError is an error Herdr reported as JSON, such as
// {"error":{"code":"pane_not_found","message":"pane w9:p9 not found"}}.
// errors.Is matches it against ErrServerNotRunning and ErrNotFound by code.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error returns the code and message.
func (e *APIError) Error() string {
	return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message)
}

// Is maps Herdr error codes onto the package sentinels.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrServerNotRunning:
		return e.Code == "server_not_running"
	case ErrNotFound:
		return e.Code == "workspace_not_found" || e.Code == "pane_not_found"
	}
	return false
}

// Client runs herdr commands against one named session.
type Client struct {
	// Bin is the herdr executable to run.
	Bin string
	// Session is the Herdr session every call targets with --session.
	Session string
}

// New returns a Client bound to the named session.
func New(session string) *Client {
	return &Client{Bin: "herdr", Session: session}
}

// inheritedEnv lists variables a process running inside a Herdr pane
// inherits from that pane. Any of them could point a call at the pane's own
// session or pane instead of the client's, so they are removed and the
// session is chosen by --session alone.
var inheritedEnv = []string{
	"HERDR_SESSION",
	"HERDR_SOCKET_PATH",
	"HERDR_WORKSPACE_ID",
	"HERDR_TAB_ID",
	"HERDR_PANE_ID",
}

// ServerStatus is the server half of "herdr status --json".
type ServerStatus struct {
	Running bool `json:"running"`
	// Version and Protocol are empty when the server is not running.
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	// Compatible reports whether this herdr binary can drive the server.
	Compatible bool `json:"compatible"`
}

// Status reports whether the session's server is running and its version.
// A stopped server is not an error.
func (c *Client) Status(ctx context.Context) (ServerStatus, error) {
	out, err := c.run(ctx, "status", "--json")
	if err != nil {
		return ServerStatus{}, err
	}
	var doc struct {
		Server struct {
			Running    bool    `json:"running"`
			Version    *string `json:"version"`
			Protocol   *int    `json:"protocol"`
			Compatible *bool   `json:"compatible"`
		} `json:"server"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return ServerStatus{}, fmt.Errorf("herdr: status: %w", err)
	}
	s := ServerStatus{Running: doc.Server.Running}
	if doc.Server.Version != nil {
		s.Version = *doc.Server.Version
	}
	if doc.Server.Protocol != nil {
		s.Protocol = *doc.Server.Protocol
	}
	if doc.Server.Compatible != nil {
		s.Compatible = *doc.Server.Compatible
	}
	return s, nil
}

// checkArg rejects a value Herdr would not read as plain data. An empty
// value or an id starting with "-" would shift the argument list. A value
// that is or starts "--session" could be taken as Herdr's own --session
// option and retarget the call, so it is refused rather than risked.
func checkArg(what, v string, id bool) error {
	switch {
	case id && (v == "" || strings.HasPrefix(v, "-")):
		return fmt.Errorf("herdr: invalid %s %q", what, v)
	case v == "--session" || strings.HasPrefix(v, "--session="):
		return fmt.Errorf("herdr: %s must not be %q", what, v)
	}
	return nil
}

// run execs herdr with args and "--session NAME", returning stdout. A
// failure carries Herdr's JSON error as an *APIError when there is one.
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	if c.Session == "" || strings.HasPrefix(c.Session, "-") {
		return "", fmt.Errorf("herdr: invalid session name %q", c.Session)
	}
	full := append(append([]string{}, args...), "--session", c.Session)
	cmd := exec.CommandContext(ctx, c.Bin, full...)
	cmd.Env = scrubEnv(os.Environ())
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return "", ErrNotInstalled
		}
		return out.String(), mapError(err, errBuf.String(), out.String())
	}
	return out.String(), nil
}

func scrubEnv(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		inherited := false
		for _, drop := range inheritedEnv {
			if name == drop {
				inherited = true
				break
			}
		}
		if !inherited {
			kept = append(kept, kv)
		}
	}
	return kept
}

// mapError turns a failed run into an error. Herdr writes server errors as
// JSON on stderr; stdout is checked too so a change of stream does not lose
// the code. Anything else, such as a usage error (exit status 2), keeps its
// text verbatim.
func mapError(err error, stderr, stdout string) error {
	for _, text := range []string{stderr, stdout} {
		if apiErr := parseAPIError(text); apiErr != nil {
			return apiErr
		}
	}
	text := strings.TrimSpace(stderr)
	if text == "" {
		text = "unknown error (empty stderr)"
	}
	return fmt.Errorf("herdr: %s: %w", text, err)
}

func parseAPIError(text string) *APIError {
	var doc struct {
		Error *APIError `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(text)), &doc) != nil || doc.Error == nil || doc.Error.Code == "" {
		return nil
	}
	return doc.Error
}

// decodeResult unmarshals the "result" member of a Herdr JSON response into
// v. what names the call for error messages.
func decodeResult(what, out string, v any) error {
	var doc struct {
		Result json.RawMessage `json:"result"`
		Error  *APIError       `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return fmt.Errorf("herdr: %s: %w", what, err)
	}
	if doc.Error != nil && doc.Error.Code != "" {
		return doc.Error
	}
	if len(doc.Result) == 0 {
		return fmt.Errorf("herdr: %s: response has no result", what)
	}
	if err := json.Unmarshal(doc.Result, v); err != nil {
		return fmt.Errorf("herdr: %s: %w", what, err)
	}
	return nil
}
