package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slorp is Slorp started as its own process on data, serving at addr, with
// nothing to watch and no model.
func slorp(ctx context.Context, data, home, addr string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-data", data, "-addr", addr, "-go", "", "-llm", "",
		"-claude", "/usr/bin/false", "-opencode", "/usr/bin/false")
	cmd.Env = append(os.Environ(), "SLORP_TEST_MAIN=1", "HOME="+home)
	return cmd
}

// loopback returns a loopback address with a port nothing was listening on.
func loopback(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// serving reports whether a Slorp answers at addr within a few seconds. One
// that answers holds its data, which it takes before it serves.
func serving(addr string) bool {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/sessions", nil)
		if err != nil {
			return false
		}
		req.Header.Set("Origin", "http://"+addr)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return true
			}
		}
	}
	return false
}

func TestOneSlorpAtATimeUsesItsData(t *testing.T) {
	data, home := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	at := loopback(t)
	first := slorp(ctx, data, home, at)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Process.Kill(); first.Wait() })
	if !serving(at) {
		t.Fatal("the first Slorp never served")
	}

	out, err := slorp(ctx, data, home, loopback(t)).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 || !strings.Contains(string(out), "another Slorp is already using "+data) {
		t.Errorf("a second Slorp on the same data = %v, %q; want it refused, saying why", err, out)
	}

	// Killed rather than stopped, as when it crashes.
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	first.Wait()
	at = loopback(t)
	third := slorp(ctx, data, home, at)
	if err := third.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { third.Process.Kill(); third.Wait() })
	if !serving(at) {
		t.Error("a Slorp started after the first was killed never served")
	}
}

func TestSlorpsOnDataNamedAlikeKeepTheirOwn(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ask := func(method, addr, path, body string) string {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "http://"+addr)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode/100 != 2 {
			t.Fatalf("%s %s = %d %s", method, path, res.StatusCode, b)
		}
		return string(b)
	}
	addrs := map[string]string{}
	start := func(name string) {
		t.Helper()
		at := loopback(t)
		cmd := slorp(ctx, filepath.Join(root, name), home, at)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		if !serving(at) {
			t.Fatalf("the Slorp on %q never served", name)
		}
		addrs[name] = at
	}

	// The second starts after the first has written, as it would read it.
	start("example space")
	ask(http.MethodPost, addrs["example space"], "/api/profiles", `{"name":"Only in example space"}`)
	start("example%20space")
	if got := ask(http.MethodGet, addrs["example%20space"], "/api/sessions", ""); strings.Contains(got, "Only in example space") {
		t.Error("a profile made on one Slorp's data shows on another's")
	}
	if got := ask(http.MethodGet, addrs["example space"], "/api/sessions", ""); !strings.Contains(got, "Only in example space") {
		t.Error("a profile made on a Slorp's data does not show on it")
	}
	for name := range addrs {
		if _, err := os.Stat(filepath.Join(root, name, "slorp.db")); err != nil {
			t.Errorf("the data in %q has no database of its own: %v", name, err)
		}
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
