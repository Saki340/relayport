// relayport: a store-and-forward file relay for unreliable links.
//
//	relayport serve   run on the relay host: web upload page + staging area + pull API
//	relayport pull    run on the destination: pulls staged files in parallel segments
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(ctx)
	case "pull":
		err = runPull(ctx)
	default:
		usage()
	}
	if err != nil && ctx.Err() == nil {
		log.Fatalf("relayport %s: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: relayport <serve|pull>

serve  (env) RELAYPORT_LISTEN=:8080 RELAYPORT_DATA=/data RELAYPORT_UPLOAD_PASSWORD=... RELAYPORT_PULL_TOKEN=...
             RELAYPORT_QUOTA=50GiB RELAYPORT_STALE_TTL=48h RELAYPORT_SECRET=... (optional, cookie signing key)
pull   (env) RELAYPORT_SERVER=https://relay.example.com RELAYPORT_PULL_TOKEN=... RELAYPORT_DEST=/data/inbox
             RELAYPORT_PARALLEL=4 RELAYPORT_SEGMENT=16MiB RELAYPORT_POLL=15s`)
	os.Exit(2)
}

// envOr returns the environment variable key, or def when unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
