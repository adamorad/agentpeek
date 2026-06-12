// Command airlock is the v2 daemon + CLI entrypoint. It routes the first
// positional argument to a subcommand: daemon (default), status, watch,
// install-service, and version.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/adamorad/airlock/internal/cli"
	"github.com/adamorad/airlock/internal/version"
)

// defaultAddr is the loopback address the daemon listens on.
const defaultAddr = "127.0.0.1:27183"

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches args (os.Args without the program name) to a subcommand and
// returns the process exit code. It is factored out of main so it can be unit
// tested.
func run(args []string) int {
	cmd := "daemon"
	rest := args
	if len(args) > 0 {
		cmd = args[0]
		rest = args[1:]
	}

	switch cmd {
	case "daemon":
		return runDaemon(rest)
	case "status":
		return cli.Status()
	case "watch":
		return cli.Watch()
	case "install-service":
		fmt.Println("install-service: not yet implemented")
		return 0
	case "version", "--version", "-v":
		fmt.Printf("%s %s\n", version.Name, version.Number)
		return 0
	default:
		usage(os.Stderr)
		return 2
	}
}

// runDaemon resolves configuration and starts the daemon. For now it prints the
// startup banner and blocks; the real MCP server is wired in by a later task.
func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbFlag := fs.String("db", "", "path to the airlock state database (default ~/.airlock/state.db)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// db path is resolved here but intentionally not opened yet.
	_ = resolveDBPath(*dbFlag)

	fmt.Printf("%s %s — starting on %s\n", version.Name, version.Number, defaultAddr)

	// Block until interrupted. A later task replaces this with the real server
	// lifecycle (mcp.Server.Start backed by an opened store). Waiting on a
	// signal keeps the process alive without a runtime deadlock and gives the
	// future server a natural shutdown hook.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return 0
}

// resolveDBPath determines the state database path using this precedence:
// the --db flag, then the AIRLOCK_DB environment variable, then the default
// of ~/.airlock/state.db.
func resolveDBPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv("AIRLOCK_DB"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Fall back to a relative path if the home dir can't be determined.
		return filepath.Join(".airlock", "state.db")
	}
	return filepath.Join(home, ".airlock", "state.db")
}

// usage writes the command summary to w.
func usage(w *os.File) {
	fmt.Fprintf(w, `%s %s

Usage:
  %s [daemon] [--db <path>]   start the daemon (default)
  %s status                   print current port state
  %s watch                    stream live port activity
  %s install-service          install the background service
  %s version                  print version

`, version.Name, version.Number, version.Name, version.Name, version.Name, version.Name, version.Name)
}
