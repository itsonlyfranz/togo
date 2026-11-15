package session

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gettogo/togo/helper/internal/agents"
	"github.com/gettogo/togo/helper/internal/herdr"
	"github.com/gettogo/togo/helper/internal/state"
	"github.com/gettogo/togo/helper/internal/tmux"
)

// TmuxFallbacks are where package managers and systems install tmux, for
// when the login PATH does not name it either. home is the user's home
// directory; when empty, the per-user folders are left out.
func TmuxFallbacks(home string) []string {
	dirs := append(agents.FallbackDirs(home), "/usr/bin", "/bin")
	list := make([]string, len(dirs))
	for i, d := range dirs {
		list[i] = filepath.Join(d, "tmux")
	}
	return list
}

// NewService returns the production Service rooted at home (see
// state.Home). It rewrites home/tmux.conf on every start so the file always
// matches this helper version. The togo socket label comes from
// TOGO_TMUX_LABEL when set, which only tests do. It does not run tmux, so a
// missing tmux surfaces on the first session call, not here.
//
// TOGO_BACKEND=herdr runs managed sessions on Herdr instead, in the Herdr
// session TOGO_HERDR_SESSION names, by default "default", the one the
// user's own herdr opens. The user's tmux sessions are still listed.
func NewService(ctx context.Context, home string) (*Service, error) {
	conf := filepath.Join(home, "tmux.conf")
	if err := state.WriteFile(conf, []byte(tmuxConf)); err != nil {
		return nil, fmt.Errorf("tmux config: %w", err)
	}
	label := cmp.Or(os.Getenv("TOGO_TMUX_LABEL"), "togo")
	userHome, _ := os.UserHomeDir()
	loginPath := state.LoginPath(ctx)
	bin := resolveTmux(loginPath, TmuxFallbacks(userHome))
	svc := &Service{
		Tmux:     &tmux.Client{Bin: bin, Socket: []string{"-L", label}, Config: conf},
		User:     &tmux.Client{Bin: bin},
		Recent:   &state.Recent{File: filepath.Join(home, "recent.json")},
		Manifest: &Manifest{Path: filepath.Join(home, "sessions.json")},
		LockFile: filepath.Join(home, "run", "bib.lock"),
		Shell:    cmp.Or(os.Getenv("SHELL"), "/bin/sh"),
		Home:     userHome,
	}
	switch name := os.Getenv("TOGO_BACKEND"); name {
	case "", "tmux":
	case "herdr":
		svc.Backend = NewHerdrBackend(home, &herdr.Client{
			Bin:     resolveHerdr(loginPath, userHome),
			Session: cmp.Or(os.Getenv("TOGO_HERDR_SESSION"), "default"),
		})
	default:
		return nil, fmt.Errorf("TOGO_BACKEND %q is not tmux or herdr", name)
	}
	return svc, nil
}

// NewHerdrBackend returns a Herdr backend driving client, with its store
// in home.
func NewHerdrBackend(home string, client HerdrClient) *HerdrBackend {
	return &HerdrBackend{
		Client:   client,
		Store:    filepath.Join(home, "herdr-sessions.json"),
		LockFile: filepath.Join(home, "run", "herdr.lock"),
	}
}

// resolveHerdr is resolveTmux for herdr, whose installer puts it in
// ~/.local/bin, which a bare ssh exec's PATH lacks.
func resolveHerdr(path, home string) string {
	dirs := agents.FallbackDirs(home)
	list := make([]string, len(dirs))
	for i, d := range dirs {
		list[i] = filepath.Join(d, "herdr")
	}
	return resolveBin("herdr", path, list)
}

// resolveTmux returns an absolute path to tmux, searching the absolute
// directories of path and then fallbacks. The helper usually runs under a
// bare ssh exec whose PATH lacks Homebrew and /usr/local, and the app runs
// attach argv under that same PATH, so a bare "tmux" would not be found
// there. When tmux is nowhere it returns "tmux", which fails as not
// installed when run.
func resolveTmux(path string, fallbacks []string) string {
	return resolveBin("tmux", path, fallbacks)
}

// resolveBin is resolveTmux for the program name.
func resolveBin(name, path string, fallbacks []string) string {
	var candidates []string
	for _, dir := range filepath.SplitList(path) {
		if filepath.IsAbs(dir) {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, c := range append(candidates, fallbacks...) {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return name
}
