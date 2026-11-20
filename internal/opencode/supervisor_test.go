package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestSupervisorParent(t *testing.T) {
	mode := os.Getenv("SLORP_TEST_PARENT")
	if mode == "" {
		return
	}
	root := os.Getenv("SLORP_TEST_ROOT")
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	if mode == "adapter" {
		a := runtimeFixture(t)
		a.Home = root
		a.ConfigDir = filepath.Join(root, "config", "opencode")
		if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		put(t, a.Bin, "#!/bin/sh\ntrap 'if test \"$OPENCODE_PURE\" != true; then if test -f \"$HOME/aborted\"; then printf abort-before-term > \"$HOME/order\"; else printf term-without-abort > \"$HOME/order\"; fi; fi; exit 0' TERM\n"+hub.ShellQuote(binary)+" -test.run='^TestNativeHelper$' -- \"$@\" &\nwait $!\n")
		if err := os.Chmod(a.Bin, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Start(ctx, hub.Launch{Dir: root}); err != nil {
			t.Fatal(err)
		}
		job := a.Hub.Sessions()[0].Job
		a.runtimeMu.Lock()
		private := a.runtimes[job].private
		a.runtimeMu.Unlock()
		put(t, filepath.Join(root, "ready"), private)
		<-ctx.Done()
		a.Shutdown()
		return
	}
	private, err := os.MkdirTemp(root, "slorp-opencode-run-")
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(private, "session.json"), `{"sessionId":"ses_fake"}`)
	child := exec.Command("/bin/sh", filepath.Join(root, "wrapper.sh"))
	child.Env = os.Environ()
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	supervisor, live, err := supervisedCommand(child, private)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	put(t, filepath.Join(root, "ready"), private)
	<-ctx.Done()
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	// The supervisor deliberately kills its own anchored process group.
	_ = supervisor.Wait()
}

func startSupervisorParent(t *testing.T, mode string) (string, *exec.Cmd, <-chan struct{}, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	put(t, filepath.Join(root, "wrapper.sh"), `#!/bin/sh
trap 'exit 0' TERM INT
/bin/sh -c 'trap "" TERM INT; echo $$ > "$1"; exec /bin/sleep 120' child "$SLORP_TEST_ROOT/descendant.pid" &
echo $$ > "$SLORP_TEST_ROOT/wrapper.pid"
wait
`)
	if mode == "exit" {
		put(t, filepath.Join(root, "wrapper.sh"), `#!/bin/sh
/bin/sh -c 'trap "" TERM INT; echo $$ > "$1"; exec /bin/sleep 120' child "$SLORP_TEST_ROOT/descendant.pid" &
echo $$ > "$SLORP_TEST_ROOT/wrapper.pid"
while ! test -s "$SLORP_TEST_ROOT/descendant.pid"; do /bin/sleep 0.01; done
exit 0
`)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestSupervisorParent$")
	cmd.Env = append(os.Environ(), "SLORP_TEST_PARENT="+mode, "SLORP_TEST_ROOT="+root, "TMPDIR="+root)
	output := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = 8 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("parent helper failed to exit")
		}
	})
	return root, cmd, done, output
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file never appeared: %s", path)
	return ""
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		// Some test containers leave adopted zombies for init to reap.
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("owned process %d is still running", pid)
}

func waitPrivateGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(supervisorGrace + 3*time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("private runtime survived cleanup deadline: %s", path)
}

func TestSupervisorCleansNonExecWrapperAndDescendantAfterParentDeath(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			root, parent, done, output := startSupervisorParent(t, "shell")
			private := waitFile(t, filepath.Join(root, "ready"))
			wrapper, err := strconv.Atoi(waitFile(t, filepath.Join(root, "wrapper.pid")))
			if err != nil {
				t.Fatal(err)
			}
			descendant, err := strconv.Atoi(waitFile(t, filepath.Join(root, "descendant.pid")))
			if err != nil {
				t.Fatal(err)
			}
			if err := parent.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("parent helper did not exit")
			}
			waitProcessGone(t, wrapper)
			waitProcessGone(t, descendant)
			if _, err := os.Stat(private); !os.IsNotExist(err) {
				t.Fatalf("private control directory survived: %v; %s", err, output.String())
			}
		})
	}
}

func TestSIGTERMParentAbortsNativeBeforeGroupTermination(t *testing.T) {
	root, parent, done, output := startSupervisorParent(t, "adapter")
	waitFile(t, filepath.Join(root, "ready"))
	if err := parent.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		t.Fatal("adapter helper did not exit")
	}
	if order := waitFile(t, filepath.Join(root, "order")); order != "abort-before-term" {
		t.Fatalf("shutdown order %q; %s", order, output.String())
	}
	b, err := os.ReadFile(filepath.Join(root, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var request struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatal(err)
		}
		found = found || request.Path == "/api/session/ses_native/interrupt"
	}
	if !found {
		t.Fatal("shutdown skipped native abort")
	}
}

func TestSIGKILLAdapterParentCleansRuntimeWithoutLosingNativeData(t *testing.T) {
	root, parent, done, _ := startSupervisorParent(t, "adapter")
	private := waitFile(t, filepath.Join(root, "ready"))
	pid, err := strconv.Atoi(waitFile(t, filepath.Join(root, "native.pid")))
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("adapter parent did not exit")
	}
	waitProcessGone(t, pid)
	waitPrivateGone(t, private)
	if _, err := os.Stat(filepath.Join(root, "native.json")); err != nil {
		t.Fatal("native conversation was removed", err)
	}
}

func TestSupervisorCleansDescendantsWhenWrapperExitsFirst(t *testing.T) {
	root, parent, done, _ := startSupervisorParent(t, "exit")
	private := waitFile(t, filepath.Join(root, "ready"))
	pid, err := strconv.Atoi(waitFile(t, filepath.Join(root, "descendant.pid")))
	if err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, pid)
	if _, err := os.Stat(private); !os.IsNotExist(err) {
		t.Fatal("private runtime survived wrapper exit", err)
	}
	if err := parent.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parent did not reap supervisor")
	}
}
