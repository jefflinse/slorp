package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestV2NativeLifecycle(t *testing.T) {
	if os.Getenv("SLORP_NATIVE_TEST") != "1" {
		t.Skip("set SLORP_NATIVE_TEST=1 to test installed OpenCode")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "PATH" && key != "TMPDIR" {
			t.Setenv(key, "")
		}
	}
	a := fixture(t)
	a.Bin = bin
	for _, key := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, filepath.Join(a.Home, key))
	}
	t.Setenv("HOME", a.Home)
	t.Setenv("OPENCODE_DISABLE_MODELS_FETCH", "true")
	t.Cleanup(a.Shutdown)
	dir := filepath.Join(a.Home, "project")
	source := `{"model":"mock/test","default_agent":"review","agents":{"review":{"mode":"primary","model":"mock/test#high"}},"providers":{"mock":{"package":"aisdk:@ai-sdk/openai-compatible","settings":{"apiKey":"test","baseURL":"http://127.0.0.1:1/v1"},"models":{"test":{"name":"Test","variants":[{"id":"high"}]}}}}}`
	put(t, filepath.Join(dir, "opencode.json"), source)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a.initLifecycle()
	options, err := a.LaunchOptions(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if options.Defaults.Model != "mock/test" || options.Defaults.AgentName != "review" || options.Defaults.Variant != "high" {
		t.Fatalf("catalog: %+v", options)
	}
	id, err := a.Start(ctx, hub.Launch{Dir: dir, Name: "V2 native lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	job := a.Hub.Sessions()[0].Job
	if err := a.Stop(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(ctx, id, dir, ""); err != nil {
		t.Fatal(err)
	}
	if len(a.Hub.Sessions()) != 1 || a.Hub.Sessions()[0].ID != id {
		t.Fatal("resume changed identity")
	}
	got, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil || string(got) != source {
		t.Fatal("source configuration changed", err)
	}
}
