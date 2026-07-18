// Command flow-mcp is fluid's MCP server. Harnesses spawn it over stdio;
// humans only ever run the subcommands (version, prune).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Cloud-Byte-Consulting/fluid/mcp"
	"github.com/Cloud-Byte-Consulting/fluid/provider"
	"github.com/Cloud-Byte-Consulting/fluid/runtime"
)

// Stamped via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	log.SetOutput(os.Stderr) // stdout is protocol-only
	log.SetPrefix("flow-mcp: ")

	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-v":
			fmt.Printf("flow-mcp %s (%s)\n", version, commit)
			return
		case "prune":
			fs := flag.NewFlagSet("prune", flag.ExitOnError)
			days := fs.Int("days", 30, "delete terminal runs older than this many days")
			fs.Parse(args[1:])
			store := &runtime.Store{Dir: stateDir()}
			removed, err := store.Prune(time.Duration(*days) * 24 * time.Hour)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("pruned %d run(s)\n", len(removed))
			return
		default:
			log.Fatalf("unknown command %q (usage: flow-mcp [version|prune])", args[0])
		}
	}
	serve()
}

func serve() {
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("state dir: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		// A blocked stdin read cannot be interrupted portably; on signal,
		// give in-flight runs a moment to journal run_cancelled, then exit.
		<-ctx.Done()
		log.Print("signal received; shutting down")
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}()

	// Background runs get their own cancelable context so both exit paths —
	// signal and stdin EOF — journal run_cancelled before the process dies.
	bg, cancelRuns := context.WithCancel(ctx)
	defer cancelRuns()

	router := provider.NewRouter()
	svc := &mcp.Service{
		Store:      &runtime.Store{Dir: dir},
		Runner:     &runtime.Runner{Dir: dir, Exec: router.Exec},
		Background: bg,
	}
	srv := mcp.NewServer(svc, version)
	log.Printf("serving MCP over stdio (state: %s, version: %s)", dir, version)
	err := srv.Serve(ctx, os.Stdin, os.Stdout)
	cancelRuns()
	time.Sleep(300 * time.Millisecond) // let runs journal run_cancelled
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func stateDir() string {
	if dir := os.Getenv("FLOW_STATE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".fluid"
	}
	return filepath.Join(home, ".fluid")
}
