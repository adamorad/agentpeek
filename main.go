// Command airlock is the v2 daemon + CLI entrypoint. It routes the first
// positional argument to a subcommand: daemon (default), status, watch,
// install-service, and version.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/adamorad/airlock/internal/cli"
	"github.com/adamorad/airlock/internal/mcp"
	"github.com/adamorad/airlock/internal/store"
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

// runDaemon resolves configuration, opens the store, assembles the managers and
// MCP handler, and serves until interrupted (SIGINT/SIGTERM).
func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbFlag := fs.String("db", "", "path to the airlock state database (default ~/.airlock/state.db)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dbPath := resolveDBPath(*dbFlag)
	s, err := store.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: open db: %v\n", version.Name, err)
		return 1
	}
	defer s.Close()

	// Assemble the managers. Presence depends on the lock manager so it can
	// release a dead agent's locks on expiry.
	lm := store.NewLockManager(s)
	pm := store.NewPresenceManager(s, lm)
	em := store.NewEventManager(s)
	tm := store.NewTaskManager(s)

	// One ctx drives both the background reapers and the HTTP server shutdown.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	lm.Start(ctx)
	pm.Start(ctx)
	tm.Start(ctx)

	// Auth posture: on darwin we rely on loopback-only binding plus Host/Origin
	// checks (single-user dev box). On other OSes (Linux/multi-user) we require a
	// bearer token stored 0600 under ~/.airlock. AIRLOCK_TOKEN overrides on any OS.
	token := ""
	tokenPath := ""
	if t := os.Getenv("AIRLOCK_TOKEN"); t != "" {
		token = t
	} else if runtime.GOOS != "darwin" {
		tokenPath = tokenFilePath()
		token, err = mcp.EnsureTokenFile(tokenPath, true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: token: %v\n", version.Name, err)
			return 1
		}
	}

	h := mcp.NewToolHandler(lm, pm, em, tm, s)
	srv := mcp.New(h, mcp.Options{Addr: defaultAddr, Token: token})

	fmt.Printf("%s %s — listening on http://%s\n", version.Name, version.Number, defaultAddr)
	if token != "" && tokenPath != "" {
		fmt.Printf("  bearer token: %s\n", tokenPath)
	}

	if err := srv.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%s: serve: %v\n", version.Name, err)
		return 1
	}
	return 0
}

// tokenFilePath returns the path to the bearer-token file (~/.airlock/token),
// falling back to a relative path if the home dir cannot be resolved.
func tokenFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".airlock", "token")
	}
	return filepath.Join(home, ".airlock", "token")
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
