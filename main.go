// Slorp is a local service that watches the agent sessions on this machine
// and serves a web UI for seeing and answering them.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/server"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:4747", "loopback address to listen on")
	ui := flag.String("ui", "index.html", "path to the web UI page")
	claudeBin := flag.String("claude", "claude", "Claude Code executable")
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

	h := hub.New()
	go (&claude.Adapter{Hub: h, Bin: *claudeBin, Home: home, Interval: 2 * time.Second}).Run(ctx)

	srv := &http.Server{Addr: *addr, Handler: server.New(h, *ui)}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("slorp listening on http://%s", *addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
