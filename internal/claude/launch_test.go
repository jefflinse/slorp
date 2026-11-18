package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a stand-in for the claude command that appends each
// call's working directory and arguments to a log, answers `agents --json`
// with the file "roster" beside it, which starts as roster, or with the file
// "stopped" when asked for the stopped sessions too, and otherwise runs body.
func fakeClaude(t *testing.T, roster, body string) (bin string, calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "claude"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\nif [ \"$1 $2 $3\" = \"agents --json --all\" ]; then cat " + filepath.Join(dir, "stopped") + " 2>/dev/null || echo '[]'; exit 0; fi\n" +
		"if [ \"$1 $2\" = \"agents --json\" ]; then cat " + filepath.Join(dir, "roster") + "; exit 0; fi\n" +
		"{ printf '%s' \"$PWD\"; for a in \"$@\"; do printf '|%s' \"$a\"; done; echo; } >> " + log + "\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roster"), []byte(roster), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

// workDir returns a directory as a command run in it names it.
func workDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStartRunsClaudeInTheBackground(t *testing.T) {
	work := workDir(t)
	roster := `[{"id":"3e14f875","kind":"background","sessionId":"3e14f875-full","state":"working"}]`
	said := "echo 'note: something first'; echo 'backgrounded · 3e14f875 · probe (idle — send a prompt to start)'; echo '  claude agents   list sessions'"
	bin, calls := fakeClaude(t, roster, said)
	a := &Adapter{Bin: bin}

	id, err := a.Start(context.Background(), work, "probe", "--version is a flag")
	if err != nil || id != "3e14f875-full" {
		t.Fatalf("Start = %q, %v; want the roster's session id for the job it printed", id, err)
	}
	if got, want := calls(), work+"|--bg|--name|probe|--|--version is a flag"; len(got) != 1 || got[0] != want {
		t.Errorf("ran %q, want %q", got, want)
	}
	select {
	case <-a.woken():
	default:
		t.Error("starting a session did not ask for the roster to be read again")
	}

	a.Plugin = "/plugins/slorp"
	if _, err := a.Start(context.Background(), work, "", "go"); err != nil {
		t.Fatal(err)
	}
	if got, want := calls()[1], work+"|--bg|--plugin-dir|/plugins/slorp|--|go"; got != want {
		t.Errorf("ran %q, want %q: no name given, and the plugin loaded", got, want)
	}
}

func TestStartWaitsForTheRosterToNameTheSession(t *testing.T) {
	// The roster lists the job with no session id at first, and gives it one
	// a moment after the command has returned.
	body := `r="$(dirname "$0")/roster"; echo '[{"id":"3e14f875"}]' > "$r"
( sleep 0.4; echo '[{"id":"3e14f875","sessionId":"3e14f875-full"}]' > "$r.next"; mv "$r.next" "$r" ) >/dev/null 2>&1 &
echo 'backgrounded · 3e14f875'`
	bin, _ := fakeClaude(t, `[]`, body)
	if id, err := (&Adapter{Bin: bin}).Start(context.Background(), t.TempDir(), "", "go"); err != nil || id != "3e14f875-full" {
		t.Errorf("Start = %q, %v; want it to wait for the roster to give the job a session id", id, err)
	}
}

func TestStartWithoutAnIDStillSucceeds(t *testing.T) {
	old := listedWait
	listedWait = 50 * time.Millisecond
	t.Cleanup(func() { listedWait = old })

	bin, _ := fakeClaude(t, `[]`, "echo 'started somehow'")
	if id, err := (&Adapter{Bin: bin}).Start(context.Background(), t.TempDir(), "", "go"); err != nil || id != "" {
		t.Errorf("Start = %q, %v; want no id and no error when claude names no job", id, err)
	}

	bin, _ = fakeClaude(t, `[{"id":"other","sessionId":"other-full"},{"id":"3e14f875"}]`, "echo 'backgrounded · 3e14f875'")
	if id, err := (&Adapter{Bin: bin}).Start(context.Background(), t.TempDir(), "", "go"); err != nil || id != "" {
		t.Errorf("Start = %q, %v; want no id and no error when the roster never gives the job a session id", id, err)
	}
}

func TestStartReportsWhatClaudeSaidWentWrong(t *testing.T) {
	bin, _ := fakeClaude(t, `[]`, "echo 'not logged in' >&2; exit 1")
	a := &Adapter{Bin: bin}
	if _, err := a.Start(context.Background(), t.TempDir(), "", "go"); err == nil || err.Error() != "not logged in" {
		t.Errorf("Start error = %v, want what claude wrote", err)
	}
	if _, err := a.Start(context.Background(), filepath.Join(t.TempDir(), "gone"), "", "go"); err == nil {
		t.Error("starting in a directory that does not exist reported success")
	}
	a.Bin = filepath.Join(t.TempDir(), "missing")
	if _, err := a.Start(context.Background(), t.TempDir(), "", "go"); err == nil {
		t.Error("a claude command that cannot run reported success")
	}
}

func TestResumeAndStop(t *testing.T) {
	work := workDir(t)
	bin, calls := fakeClaude(t, `[{"id":"3e14f875","sessionId":"sid"}]`, "echo 'backgrounded · 3e14f875'")
	a := &Adapter{Bin: bin, Plugin: "/plugins/slorp"}

	if err := a.Resume(context.Background(), "sid", work, ""); err != nil {
		t.Fatal(err)
	}
	// Once it has run in the background it keeps its own options.
	stopped := `[{"id":"3e14f875","kind":"background","sessionId":"sid","state":"done"}]`
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "stopped"), []byte(stopped), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(context.Background(), "sid", work, "carry on"); err != nil {
		t.Fatal(err)
	}
	<-a.woken()
	if err := a.Stop(context.Background(), "3e14f875"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.woken():
	default:
		t.Error("stopping a session did not ask for the roster to be read again")
	}
	got := calls()
	want := []string{
		work + "|--bg|--resume|sid|--plugin-dir|/plugins/slorp",
		work + "|--bg|--resume|sid|--|carry on",
		"|stop|3e14f875",
	}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || !strings.HasSuffix(got[2], want[2]) {
		t.Errorf("ran %q, want %q", got, want)
	}

	bin, _ = fakeClaude(t, `[]`, "echo \"No session matching '$2'.\"; exit 1")
	if err := (&Adapter{Bin: bin}).Stop(context.Background(), "nope"); err == nil || err.Error() != "No session matching 'nope'." {
		t.Errorf("Stop error = %v, want what claude printed", err)
	}
}

func TestBackgrounded(t *testing.T) {
	for out, want := range map[string]string{
		"backgrounded · 32a47c47 · slorp-probe-1\n  claude agents\n":             "32a47c47",
		"backgrounded · e9aeb669\n":                                              "e9aeb669",
		"note: woke session 3e14f875.\nbackgrounded · 3e14f875 · p (idle — x)\n": "3e14f875",
		"note: session 32a47c47 is already running\n":                            "",
		"": "",
	} {
		if got := backgrounded(out); got != want {
			t.Errorf("backgrounded(%q) = %q, want %q", out, got, want)
		}
	}
}
