package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// slorp is Slorp started as its own process on data, with nothing to watch
// and no model.
func slorp(ctx context.Context, data, home string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-data", data, "-addr", "127.0.0.1:0", "-go", "", "-llm", "",
		"-claude", "/usr/bin/false", "-opencode", "/usr/bin/false")
	cmd.Env = append(os.Environ(), "SLORP_TEST_MAIN=1", "HOME="+home)
	return cmd
}

// heldElsewhere waits until another process holds data, and reports whether
// one did within a few seconds.
func heldElsewhere(t *testing.T, data string) bool {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		f, err := os.Open(filepath.Join(data, "slorp.lock"))
		if err != nil {
			continue
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true
		}
	}
	return false
}

func TestOneSlorpAtATimeUsesItsData(t *testing.T) {
	data, home := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	first := slorp(ctx, data, home)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Process.Kill(); first.Wait() })
	if !heldElsewhere(t, data) {
		t.Fatal("the first Slorp never held its data")
	}

	out, err := slorp(ctx, data, home).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 || !strings.Contains(string(out), "another Slorp is already using "+data) {
		t.Errorf("a second Slorp on the same data = %v, %q; want it refused, saying why", err, out)
	}

	// Killed rather than stopped, as when it crashes.
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	first.Wait()
	third := slorp(ctx, data, home)
	if err := third.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { third.Process.Kill(); third.Wait() })
	if !heldElsewhere(t, data) {
		t.Error("a Slorp started after the first was killed never held the data")
	}
}

// TestMain runs Slorp itself when SLORP_TEST_MAIN is set, so a test can start
// it as its own process.
func TestMain(m *testing.M) {
	if os.Getenv("SLORP_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestARefusedModelAddressIsNotRepeatedOrActedOn(t *testing.T) {
	const secret = "s3cr3t-sentinel"
	for _, llm := range []string{"http://user:" + secret + "@localhost:1234", "ftp://" + secret + "/", "http://local host/" + secret} {
		data, home := t.TempDir(), t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-data", data, "-addr", "127.0.0.1:0", "-go", "", "-llm", llm)
		cmd.Env = append(os.Environ(), "SLORP_TEST_MAIN=1", "HOME="+home)
		out, err := cmd.CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Errorf("starting with -llm %q was still running after 30 seconds, want it refused", llm)
		} else if !errors.As(err, &exit) || exit.ExitCode() == 0 {
			t.Errorf("starting with -llm %q = %v, want a nonzero exit", llm, err)
		}
		if said := string(out); !strings.Contains(said, "-llm: the model server's address") || strings.Contains(said, secret) {
			t.Errorf("starting with -llm %q said %q; want what is wrong with -llm, without repeating it", llm, said)
		}
		for _, dir := range []string{data, home} {
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Errorf("starting with -llm %q left %v in %s, %v; want it untouched", llm, entries, dir, err)
			}
		}
	}
}
