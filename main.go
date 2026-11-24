// Slorp is a local service that watches the agent sessions on this machine
// and serves a web UI for seeing and answering them.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/enrich"
	"github.com/jefflinse/slorp/internal/feed"
	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/opencode"
	"github.com/jefflinse/slorp/internal/release"
	"github.com/jefflinse/slorp/internal/server"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/terminal"
)

//go:embed index.html
var page []byte

// importJSON carries a knowledge base from the JSON file earlier versions
// wrote into the database, once, leaving the file in place.
func importJSON(st *store.Store, path string) {
	var existing json.RawMessage
	if found, err := st.Get("kb", "main", &existing); found || err != nil {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		log.Printf("knowledge base: %s is not valid JSON, not imported", path)
		return
	}
	if err := st.Put("kb", "main", doc); err != nil {
		log.Printf("knowledge base: import failed: %v", err)
		return
	}
	log.Printf("knowledge base: imported %s", path)
}

// holdData keeps dir for this process alone until it closes the file
// returned or ends, however it ends, and fails at once when another process
// has it.
func holdData(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "slorp.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another Slorp is already using %s", dir)
		}
		return nil, err
	}
	return f, nil
}

func main() {
	exe, restart := run()
	if !restart {
		return
	}
	log.Printf("slorp restarting as %s", exe)
	log.Fatalf("restart: %v", syscall.Exec(exe, os.Args, os.Environ()))
}

// run serves Slorp until it is stopped, and returns its executable and
// whether to start it again, as it is to be once upgraded.
func run() (exe string, restart bool) {
	addr := flag.String("addr", "127.0.0.1:4747", "loopback address to listen on")
	ui := flag.String("ui", "", "path to a web UI page to serve in place of the one built in, read afresh for each request")
	claudeBin := flag.String("claude", "claude", "Claude Code executable")
	opencodeBin := flag.String("opencode", "opencode", "OpenCode executable")
	goBin := flag.String("go", "go", "Go command, which looks for newer releases of Slorp and installs them; empty never looks")
	llm := flag.String("llm", enrich.DefaultURL, "OpenAI-compatible local model server, in place of the one chosen in Settings; empty runs without a model")
	model := flag.String("model", "", "model to summarize with, in place of the one chosen in Settings (default: whichever one is loaded)")
	data := flag.String("data", "", "directory for Slorp's database (default: ~/.slorp)")
	plugin := flag.String("plugin", "", "directory of the Slorp plugin to load into sessions started here; empty when Claude Code has it installed")
	flag.Parse()

	host, _, err := net.SplitHostPort(*addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		log.Fatalf("-addr must be a loopback address, got %q", *addr)
	}
	started := enrich.Startup{URL: *llm, Model: *model}
	flag.Visit(func(f *flag.Flag) {
		started.URLGiven = started.URLGiven || f.Name == "llm"
		started.ModelGiven = started.ModelGiven || f.Name == "model"
	})
	if started, err = started.Checked(); err != nil {
		log.Fatalf("-llm: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *data == "" {
		*data = filepath.Join(home, ".slorp")
	}
	if *data, err = filepath.Abs(*data); err != nil {
		log.Fatal(err)
	}
	// Sessions start in other directories, where a relative path would not hold.
	if *plugin != "" {
		if *plugin, err = filepath.Abs(*plugin); err != nil {
			log.Fatalf("-plugin: %v", err)
		}
	}
	held, err := holdData(*data)
	if err != nil {
		log.Fatalf("data: %v", err)
	}
	defer held.Close()
	st, err := store.Open(filepath.Join(*data, "slorp.db"))
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer st.Close()
	importJSON(st, filepath.Join(*data, "kb.json"))
	k, err := kb.Open(st)
	if err != nil {
		log.Fatalf("knowledge base: %v", err)
	}

	h, err := hub.Open(k, st)
	if err != nil {
		log.Fatalf("sessions: %v", err)
	}
	h.Rostered(claude.Agent)
	tools := integrations.NewManager(func() map[string]bool { return h.Settings().Integrations }, integrations.GitHub(integrations.Exec))
	transcripts := &claude.Adapter{Hub: h, Bin: *claudeBin, Home: home, Interval: 5 * time.Second, Plugin: *plugin, URL: "http://" + *addr, Updates: 24 * time.Hour}
	go transcripts.Run(ctx)
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	oc := &opencode.Adapter{Hub: h, Interval: 2 * time.Second, Silence: 30 * time.Second,
		Bin: *opencodeBin, Home: home, Data: *data, URL: "http://" + *addr,
		ConfigDir: filepath.Join(configHome, "opencode"), ExtraConfigDir: os.Getenv("OPENCODE_CONFIG_DIR"), ConfigFile: os.Getenv("OPENCODE_CONFIG")}
	defer func() { stop(); oc.Shutdown() }()
	go oc.Run(ctx)
	summarizer := &enrich.Enricher{Hub: h, Resolve: started.Config, Offer: tools.Offer}
	go summarizer.Run(ctx)

	version := release.Installed()
	if exe, err = os.Executable(); err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		log.Printf("release: Slorp cannot tell where it runs from, so it will not look for newer releases: %v", err)
		version = ""
	}
	h.SetRelease(hub.Release{Version: version})
	var again atomic.Bool
	releases := &release.Checker{Version: version, Exe: exe, Go: *goBin, Every: 24 * time.Hour, Report: h.SetRelease,
		Relaunch: func() { again.Store(true); stop() }}
	go releases.Run(ctx)

	poller := feed.New(h, tools, func() (dirs []string) {
		for _, e := range k.Efforts() {
			dirs = append(dirs, e.Dirs...)
		}
		return dirs
	})
	go poller.Run(ctx)

	history := map[string]server.History{
		claude.Agent: {
			Exchanges: func(_ context.Context, id string, limit int) ([]hub.Exchange, error) {
				return transcripts.Exchanges(id, limit)
			},
			Reply: func(_ context.Context, id, ends string) (string, error) { return transcripts.Reply(id, ends) },
		},
		opencode.Agent: {Exchanges: oc.Exchanges, Reply: oc.Reply},
	}
	srv := &http.Server{Addr: *addr, Handler: server.New(h, k, tools, poller, history, map[string]server.Launcher{claude.Agent: transcripts, opencode.Agent: oc}, map[string]server.PluginManager{claude.Agent: transcripts, opencode.Agent: oc}, map[string]server.Delegates{claude.Agent: transcripts}, summarizer, terminal.New(), releases, server.Page(page, *ui))}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("slorp listening on http://%s", *addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Printf("server: %v", err)
	}
	return exe, again.Load()
}
