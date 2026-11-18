// Slorp is a local service that watches the agent sessions on this machine
// and serves a web UI for seeing and answering them.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/enrich"
	"github.com/jefflinse/slorp/internal/feed"
	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/opencode"
	"github.com/jefflinse/slorp/internal/server"
	"github.com/jefflinse/slorp/internal/store"
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

func main() {
	addr := flag.String("addr", "127.0.0.1:4747", "loopback address to listen on")
	ui := flag.String("ui", "", "path to a web UI page to serve in place of the one built in, read afresh for each request")
	claudeBin := flag.String("claude", "claude", "Claude Code executable")
	llm := flag.String("llm", "http://localhost:1234", "OpenAI-compatible local model server; empty disables summaries")
	model := flag.String("model", "", "model to summarize with (default: whichever one is loaded)")
	data := flag.String("data", "", "directory for Slorp's database (default: ~/.slorp)")
	plugin := flag.String("plugin", "", "directory of the Slorp plugin to load into sessions started here; empty when Claude Code has it installed")
	flag.Parse()

	host, _, err := net.SplitHostPort(*addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		log.Fatalf("-addr must be a loopback address, got %q", *addr)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *data == "" {
		*data = filepath.Join(home, ".slorp")
	}
	// Sessions start in other directories, where a relative path would not hold.
	if *plugin != "" {
		if *plugin, err = filepath.Abs(*plugin); err != nil {
			log.Fatalf("-plugin: %v", err)
		}
	}
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

	h := hub.New(k, st)
	h.Rostered(claude.Agent)
	tools := integrations.NewManager(func() map[string]bool { return h.Settings().Integrations }, integrations.GitHub(integrations.Exec))
	transcripts := &claude.Adapter{Hub: h, Bin: *claudeBin, Home: home, Interval: 5 * time.Second, Plugin: *plugin, URL: "http://" + *addr}
	go transcripts.Run(ctx)
	go (&opencode.Adapter{Hub: h, Interval: 2 * time.Second, Silence: 30 * time.Second}).Run(ctx)
	if *llm != "" {
		go (&enrich.Enricher{Hub: h, BaseURL: *llm, Model: *model, Offer: tools.Offer}).Run(ctx)
	} else {
		h.SetSummarizer(hub.Summarizer{Off: true})
	}

	poller := feed.New(h, tools, func() (dirs []string) {
		for _, e := range k.Efforts() {
			dirs = append(dirs, e.Dirs...)
		}
		return dirs
	})
	go poller.Run(ctx)

	srv := &http.Server{Addr: *addr, Handler: server.New(h, k, tools, poller, map[string]server.History{claude.Agent: transcripts.Exchanges}, map[string]server.Launcher{claude.Agent: transcripts}, server.Page(page, *ui))}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("slorp listening on http://%s", *addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
