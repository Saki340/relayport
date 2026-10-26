// inbox-relay: a store-and-forward file relay for unreliable links.
//
//	inbox-relay serve   run on the relay host: web upload page + staging area + pull API
//	inbox-relay pull    run on the destination: pulls staged files in parallel segments
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
		log.Fatalf("inbox-relay %s: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: inbox-relay <serve|pull>

serve  (env) IR_LISTEN=:8080 IR_DATA=/data IR_UPLOAD_PASSWORD=... IR_PULL_TOKEN=...
             IR_QUOTA=50GiB IR_STALE_TTL=48h IR_SECRET=... (optional, cookie signing key)
pull   (env) IR_SERVER=https://relay.example.com IR_PULL_TOKEN=... IR_DEST=/data/inbox
             IR_PARALLEL=4 IR_SEGMENT=16MiB IR_POLL=15s`)
	os.Exit(2)
}

// envOr returns the environment variable key, or def when unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
