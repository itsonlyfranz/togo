package herdr_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gettogo/togo/helper/internal/herdr"
)

// TestMain lets the test binary stand in for herdr: with TOGO_FAKE_HERDR
// set it records its argv and the HERDR_* environment it was given, then
// replies with the canned stdout, stderr and exit status.
func TestMain(m *testing.M) {
	if dir := os.Getenv("TOGO_FAKE_HERDR"); dir != "" {
		os.Exit(fakeHerdr(dir))
	}
	os.Exit(m.Run())
}

type fakeCall struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

func fakeHerdr(dir string) int {
	env := map[string]string{}
	for _, k := range []string{"HERDR_SESSION", "HERDR_SOCKET_PATH", "HERDR_WORKSPACE_ID", "HERDR_TAB_ID", "HERDR_PANE_ID"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	rec, _ := json.Marshal(fakeCall{Args: os.Args[1:], Env: env})
	f, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintf(f, "%s\n", rec)
		f.Close()
	}
	reply := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return string(b)
	}
	fmt.Fprint(os.Stdout, reply("stdout"))
	fmt.Fprint(os.Stderr, reply("stderr"))
	code, _ := strconv.Atoi(reply("exit"))
	return code
}

// fake is a herdr stand-in for one test.
type fake struct {
	t   *testing.T
	dir string
}

// newFake returns a Client on session "togo-test" whose binary is the fake,
// replying with an empty success until told otherwise.
func newFake(t *testing.T) (*herdr.Client, *fake) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	f := &fake{t: t, dir: t.TempDir()}
	t.Setenv("TOGO_FAKE_HERDR", f.dir)
	return &herdr.Client{Bin: exe, Session: "togo-test"}, f
}

// reply sets what the next calls print and their exit status.
func (f *fake) reply(stdout, stderr string, exit int) {
	f.t.Helper()
	for name, v := range map[string]string{"stdout": stdout, "stderr": stderr, "exit": strconv.Itoa(exit)} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(v), 0o600); err != nil {
			f.t.Fatalf("write %s: %v", name, err)
		}
	}
}

// calls returns every invocation so far.
func (f *fake) calls() []fakeCall {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("read calls: %v", err)
	}
	var out []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var c fakeCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			f.t.Fatalf("bad call record %q: %v", line, err)
		}
		out = append(out, c)
	}
	return out
}

// onlyArgs returns the argv of the only call made, failing otherwise.
func (f *fake) onlyArgs() []string {
	f.t.Helper()
	calls := f.calls()
	if len(calls) != 1 {
		f.t.Fatalf("herdr ran %d times, want once: %v", len(calls), calls)
	}
	return calls[0].Args
}
