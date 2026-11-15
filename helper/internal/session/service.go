// Package session is the helper's domain layer for terminal sessions: it
// creates, lists, renames and kills managed sessions, which run on a
// Backend (tmux on the togo socket unless one is set), and shows the user's
// own tmux sessions read-only.
package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gettogo/togo/helper/internal/state"
	"github.com/gettogo/togo/helper/internal/tmux"
	"github.com/gettogo/togo/helper/internal/wire"
)

// ErrNotFound is returned when no session has the given id or bib.
var ErrNotFound = errors.New("session not found")

// ErrNotManaged is returned by write calls on an unmanaged session.
var ErrNotManaged = errors.New("session not managed")

const (
	requiredMajor = 3
	requiredMinor = 2

	statusIdle   = "idle"
	statusExited = "exited"
)

// TmuxUnavailableError means tmux is missing or older than 3.2.
type TmuxUnavailableError struct {
	// Found is the reported version, or "" when tmux is not installed.
	Found string
}

// Error names the required version and what was found.
func (e *TmuxUnavailableError) Error() string {
	if e.Found == "" {
		return "tmux 3.2 or newer is required, but tmux is not installed"
	}
	return fmt.Sprintf("tmux 3.2 or newer is required, found %s", e.Found)
}

// InvalidParamsError is a rejected input. Msg is safe to show to the user.
// Reason, when set, is the protocol's data.reason word for the rejection
// (wire.ReasonArgs and the like); otherwise Msg is reported.
type InvalidParamsError struct {
	Msg    string
	Reason string
}

// Error returns Msg.
func (e *InvalidParamsError) Error() string { return e.Msg }

// Session is the protocol v1 Session object.
type Session struct {
	ID              string    `json:"id"`
	Bib             *string   `json:"bib"`
	Name            string    `json:"name"`
	Agent           string    `json:"agent"`
	Cwd             string    `json:"cwd"`
	CreatedAt       time.Time `json:"createdAt"`
	Status          string    `json:"status"`
	Activity        *string   `json:"activity"`
	AttachedClients int       `json:"attachedClients"`
	Managed         bool      `json:"managed"`
	// Muted is read from the session's @togo_muted option, so it outlives
	// the process that set it.
	Muted bool             `json:"muted"`
	Meta  wire.SessionMeta `json:"meta"`
	// Mission is the harness summary (harness.v1), set by the daemon for
	// sessions an adapter serves.
	Mission *wire.MissionSummary `json:"mission,omitempty"`
	// Command is the agent's argv; nil for the shell and unmanaged sessions.
	Command []string `json:"command"`
	// Exit is how the agent ended while Status is ended or failed, else nil.
	Exit *wire.SessionExit `json:"exit"`
	// ActivityHistory is the recent output volume of a managed session, 16
	// values in 0...1, oldest first, each covering 30 seconds; the hub
	// fills it. Omitted when unknown.
	ActivityHistory []float64 `json:"activityHistory,omitempty"`
	// SavedExit is the exit stored in the session's @togo_exit option. It
	// is not sent: the hub, which owns the exit, reads it after a restart.
	SavedExit *SavedExit `json:"-"`
	// ExitRestored is set by the hub on a session whose Exit it read back
	// after a restart, or found when it started, rather than saw happen:
	// it was announced before, or happened while nothing was watching.
	ExitRestored bool `json:"-"`
}

// Service runs session operations. Construct it with NewService, or set the
// fields directly in tests. A Service must not be copied after first use.
type Service struct {
	// Tmux is the client for the togo socket, where managed sessions live
	// unless Backend is set.
	Tmux *tmux.Client
	// Backend runs managed sessions; nil means tmux on Tmux.
	Backend Backend
	// User is the client for the user's default socket, listed read-only.
	// Nil disables unmanaged sessions.
	User *tmux.Client
	// Recent records the folders sessions are created in.
	Recent *state.Recent
	// Manifest records managed sessions so a restart can offer them
	// again. Nil records nothing.
	Manifest *Manifest
	// LockFile serializes bib allocation across processes.
	LockFile string
	// Shell runs agent commands; empty means /bin/sh.
	Shell string
	// Path is searched for agent commands; empty means state.LoginPath.
	Path string
	// Home is the user's home directory. With it, agents missing from Path
	// are also looked up in agents.FallbackDirs(Home).
	Home string
	// TogoPath is the helper a session runs to report its agent's exit.
	TogoPath string
	// ShimDir holds the open, xdg-open and BROWSER shims that send web pages
	// to the phone. Empty leaves a session's BROWSER and PATH alone.
	ShimDir string
	// Logf receives problems with stored session state. Nil discards them.
	Logf func(format string, args ...any)
	// NewID and Now are test seams; nil means a random id and time.Now.
	NewID func() string
	Now   func() time.Time

	mu       sync.Mutex
	checked  bool
	checkErr error
}

// List returns managed sessions ordered by bib, then unmanaged sessions
// ordered by name.
func (s *Service) List(ctx context.Context) ([]Session, error) {
	if err := s.checkBackend(ctx); err != nil {
		return nil, err
	}
	managed, err := s.listManaged(ctx)
	if err != nil {
		return nil, err
	}
	return append(managed, s.listUnmanaged(ctx)...), nil
}

func (s *Service) listManaged(ctx context.Context) ([]Session, error) {
	recs, err := s.backend().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	out := []Session{}
	for _, r := range recs {
		// Anything else on the togo socket was not made by Create, for
		// example by someone running tmux -L togo by hand, and could not
		// be described as a valid managed session.
		if managedIDPattern.MatchString(r.ID) && validBib(r.Values[optionBib]) {
			out = append(out, s.withExit(s.withCommand(managedSession(r), r.Values[optionArgv]), r.Values[optionExit]))
		}
	}
	slices.SortFunc(out, func(a, b Session) int { return strings.Compare(*a.Bib, *b.Bib) })
	return out, nil
}

// listUnmanaged returns the user's default-socket sessions. A failing
// default server counts as having none.
func (s *Service) listUnmanaged(ctx context.Context) []Session {
	if s.User == nil {
		return nil
	}
	infos, err := s.User.ListSessions(ctx)
	if err != nil {
		// The user's own server is outside the helper's control (an old
		// server left running across a tmux upgrade fails every command),
		// and must not hide the managed sessions.
		return nil
	}
	slices.SortFunc(infos, func(a, b tmux.SessionInfo) int { return strings.Compare(a.Name, b.Name) })
	out := make([]Session, 0, len(infos))
	for _, info := range infos {
		out = append(out, unmanagedSession(info))
	}
	return out
}

// Kill ends a managed session.
func (s *Service) Kill(ctx context.Context, id string) error {
	if err := requireManaged(id); err != nil {
		return err
	}
	if err := s.checkBackend(ctx); err != nil {
		return err
	}
	err := s.backend().Kill(ctx, id)
	if err == nil || isNoSession(err) {
		s.forget(id)
	}
	if err != nil {
		return notFoundOr(err, id)
	}
	return nil
}

// Rename sets a managed session's display name and returns the session.
// The name follows Create's rules, except that blank is rejected.
func (s *Service) Rename(ctx context.Context, id, name string) (Session, error) {
	if err := requireManaged(id); err != nil {
		return Session{}, err
	}
	name, err := normalizeName(name)
	if err != nil {
		return Session{}, err
	}
	if name == "" {
		return Session{}, &InvalidParamsError{Msg: "name must not be empty"}
	}
	if err := s.checkBackend(ctx); err != nil {
		return Session{}, err
	}
	if err := s.backend().SetValue(ctx, id, optionName, name); err != nil {
		return Session{}, notFoundOr(err, id)
	}
	sess, err := s.find(ctx, id)
	if err == nil {
		s.rememberName(sess)
	}
	return sess, err
}

// SetMuted records whether pushes for a managed session are suppressed and
// returns the session.
func (s *Service) SetMuted(ctx context.Context, id string, muted bool) (Session, error) {
	if err := requireManaged(id); err != nil {
		return Session{}, err
	}
	if err := s.checkBackend(ctx); err != nil {
		return Session{}, err
	}
	value := "0"
	if muted {
		value = "1"
	}
	if err := s.backend().SetValue(ctx, id, optionMuted, value); err != nil {
		return Session{}, notFoundOr(err, id)
	}
	s.rememberMuted(id, muted)
	return s.find(ctx, id)
}

// rememberMuted updates a recorded session's mute, so a restore keeps it.
func (s *Service) rememberMuted(id string, muted bool) {
	if s.Manifest == nil {
		return
	}
	err := s.Manifest.update(func(list []ManifestEntry) []ManifestEntry {
		for i := range list {
			if list[i].ID == id {
				list[i].Muted = muted
			}
		}
		return list
	})
	if err != nil {
		s.logf("session record %s: %v", id, err)
	}
}

// AttachCommand returns the argv that attaches a terminal to the session.
// It works for unmanaged sessions too, since attaching changes nothing.
func (s *Service) AttachCommand(ctx context.Context, id string) ([]string, error) {
	sess, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.Managed {
		argv, err := s.backend().AttachArgv(ctx, sess.ID)
		if err != nil {
			return nil, notFoundOr(err, id)
		}
		return argv, nil
	}
	return s.User.AttachArgv(sess.Name), nil
}

// PhoneClientEnv is set in a managed session's environment when the phone
// attaches, so shells and agents started in new panes can tell a phone is
// connected. Panes that already exist keep the environment they started with.
const PhoneClientEnv = "TOGO_CLIENT"

// PhoneAttachCommand is AttachCommand for the phone. An attach command
// cannot carry an environment into the session's panes, so for a managed
// session it first sets TOGO_CLIENT=1 in the session environment, where the
// backend can. The user's own sessions are attached to unchanged.
func (s *Service) PhoneAttachCommand(ctx context.Context, id string) ([]string, error) {
	sess, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if !sess.Managed {
		return s.User.AttachArgv(sess.Name), nil
	}
	b := s.backend()
	if err := b.SetEnv(ctx, sess.ID, PhoneClientEnv, "1"); err != nil {
		if isNoSession(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("set environment: %w", err)
	}
	argv, err := b.AttachArgv(ctx, sess.ID)
	if err != nil {
		return nil, notFoundOr(err, id)
	}
	return argv, nil
}

// Resolve finds a session by id or by bib, written with or without the
// leading zero ("4" or "04").
func (s *Service) Resolve(ctx context.Context, bibOrID string) (Session, error) {
	if _, err := ValidateID(bibOrID); err == nil {
		return s.find(ctx, bibOrID)
	}
	n, err := strconv.Atoi(bibOrID)
	if !isBibNumber(bibOrID) || err != nil || n < 1 {
		return Session{}, &InvalidParamsError{Msg: fmt.Sprintf("%q is not a session number or id", bibOrID)}
	}
	bib := fmt.Sprintf("%02d", n)
	if err := s.checkBackend(ctx); err != nil {
		return Session{}, err
	}
	sessions, err := s.listManaged(ctx)
	if err != nil {
		return Session{}, err
	}
	for _, sess := range sessions {
		if *sess.Bib == bib {
			return sess, nil
		}
	}
	return Session{}, fmt.Errorf("%w: number %s", ErrNotFound, bib)
}

// find looks id up on the one socket it can live on, so a broken default
// server never hides a managed session.
func (s *Service) find(ctx context.Context, id string) (Session, error) {
	managed, err := ValidateID(id)
	if err != nil {
		return Session{}, err
	}
	if err := s.checkBackend(ctx); err != nil {
		return Session{}, err
	}
	var sessions []Session
	if managed {
		if sessions, err = s.listManaged(ctx); err != nil {
			return Session{}, err
		}
	} else {
		sessions = s.listUnmanaged(ctx)
	}
	for _, sess := range sessions {
		if sess.ID == id {
			return sess, nil
		}
	}
	return Session{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// checkBackend checks the backend can run sessions, which for tmux is its
// version. Answers the backend calls stable (for tmux: missing, too old,
// supported) are kept for the life of the Service; others, such as a
// cancelled context, are asked again next call.
func (s *Service) checkBackend(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checked {
		return s.checkErr
	}
	stable, err := s.backend().Check(ctx)
	if !stable {
		return err
	}
	s.checked, s.checkErr = true, err
	return err
}

func trimTmux(raw string) string { return strings.TrimPrefix(raw, "tmux ") }

func (s *Service) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	return newID()
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) shell() string {
	return cmp.Or(s.Shell, "/bin/sh")
}

// requireManaged rejects ids that are malformed or unmanaged from the id
// alone, before any tmux call.
func requireManaged(id string) error {
	managed, err := ValidateID(id)
	if err != nil {
		return err
	}
	if !managed {
		return fmt.Errorf("%w: %s", ErrNotManaged, id)
	}
	return nil
}

func notFoundOr(err error, id string) error {
	if isNoSession(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return fmt.Errorf("session %s: %w", id, err)
}

func managedSession(r Record) Session {
	created := r.Created.UTC()
	if t, err := time.Parse(time.RFC3339, r.Values[optionCreated]); err == nil {
		created = t.UTC()
	}
	bib := r.Values[optionBib]
	st := statusIdle
	if r.Dead {
		st = statusExited
	}
	return Session{
		ID:              r.ID,
		Bib:             &bib,
		Name:            r.Values[optionName],
		Agent:           r.Values[optionAgent],
		Cwd:             r.Path,
		CreatedAt:       created,
		Status:          st,
		AttachedClients: r.Attached,
		Managed:         true,
		Muted:           r.Values[optionMuted] == "1",
	}
}

func unmanagedSession(info tmux.SessionInfo) Session {
	return Session{
		ID:              unmanagedID(info.Name),
		Name:            info.Name,
		Agent:           "shell",
		Cwd:             info.Path,
		CreatedAt:       info.Created.UTC(),
		Status:          status(info),
		AttachedClients: info.Attached,
	}
}

func status(info tmux.SessionInfo) string {
	if info.Dead {
		return statusExited
	}
	return statusIdle
}
