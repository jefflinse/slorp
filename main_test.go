package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

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
		cmd := exec.Command(os.Args[0], "-data", data, "-addr", "127.0.0.1:0", "-go", "", "-llm", llm)
		cmd.Env = append(os.Environ(), "SLORP_TEST_MAIN=1", "HOME="+home)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() == 0 {
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
