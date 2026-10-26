// relayport: a store-and-forward file relay for unreliable links.
//
//	relayport serve   run on the relay host: web upload page + staging area + pull API
//	relayport pull    run on the destination: pulls staged files in parallel segments
//	relayport push    upload files or folders to a relay from the command line
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = ""
)

func versionString() string {
	if commit == "" {
		return "relayport " + version
	}
	return fmt.Sprintf("relayport %s (%s)", version, commit)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		log.Print(versionString())
		err = runServe(ctx)
	case "pull":
		log.Print(versionString())
		err = runPull(ctx)
	case "push":
		err = runPush(ctx, os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(versionString())
	default:
		usage()
	}
	if err != nil && ctx.Err() == nil {
		log.Fatalf("relayport %s: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: relayport <serve|pull|push|version>

serve  (env) RELAYPORT_LISTEN=:8080 RELAYPORT_DATA=/data RELAYPORT_UPLOAD_PASSWORD=... RELAYPORT_PULL_TOKEN=...
             RELAYPORT_QUOTA=50GiB RELAYPORT_STALE_TTL=48h RELAYPORT_SECRET=... (optional, cookie signing key)
pull   (env) RELAYPORT_SERVER=https://relay.example.com RELAYPORT_PULL_TOKEN=... RELAYPORT_DEST=/data/inbox
             RELAYPORT_PARALLEL=4 RELAYPORT_SEGMENT=16MiB RELAYPORT_POLL=15s
push   [-to dir] path...
       (env) RELAYPORT_SERVER=https://relay.example.com RELAYPORT_UPLOAD_PASSWORD=... RELAYPORT_PARALLEL=4`)
	os.Exit(2)
}

// envOr returns the environment variable key, or def when unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// serverURL returns RELAYPORT_SERVER without a trailing slash, insisting on a scheme
// so that "203.0.113.5:8080" fails clearly instead of as a confusing request error.
func serverURL() (string, error) {
	v := strings.TrimRight(os.Getenv("RELAYPORT_SERVER"), "/")
	switch {
	case v == "":
		return "", fmt.Errorf("RELAYPORT_SERVER must be set (e.g. https://relay.example.com or http://203.0.113.5:8080)")
	case !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://"):
		return "", fmt.Errorf("RELAYPORT_SERVER %q must start with http:// or https://", v)
	}
	return v, nil
}

// requireSecret returns the value of a mandatory secret, refusing the
// placeholders shipped in the example compose files.
func requireSecret(key string) (string, error) {
	v := os.Getenv(key)
	switch {
	case v == "":
		return "", fmt.Errorf("%s must be set", key)
	case strings.HasPrefix(strings.ToLower(v), "change-me"):
		return "", fmt.Errorf("%s is still the example placeholder; set a real value (e.g. openssl rand -hex 32)", key)
	}
	return v, nil
}
