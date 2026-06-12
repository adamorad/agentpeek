// Package cli implements the human-facing subcommands (status, watch) that talk
// to a running airlock daemon and render its state to the terminal.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/adamorad/airlock/internal/store"
)

// defaultAddr mirrors the loopback address the daemon listens on. It is only
// used for the title line in rendered output.
const defaultAddr = "127.0.0.1:27183"

// Status prints a one-shot snapshot of the daemon's coordination state at
// dbPath and returns a process exit code. If no state database exists yet it
// prints a friendly hint and returns 0 (nothing to show is not an error).
func Status(dbPath string) int {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr,
			"No Airlock state found at %s — is the daemon running? (start it with: airlock daemon)\n",
			dbPath)
		return 0
	}

	s, err := store.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: open db: %v\n", err)
		return 1
	}
	defer s.Close()

	out, err := renderSnapshot(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: render: %v\n", err)
		return 1
	}
	fmt.Print(out)
	return 0
}

// Watch clears the screen and reprints the coordination snapshot at dbPath once
// per second until interrupted (SIGINT/SIGTERM), then returns a process exit
// code. If no state database exists it behaves like Status (prints a hint).
func Watch(dbPath string) int {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr,
			"No Airlock state found at %s — is the daemon running? (start it with: airlock daemon)\n",
			dbPath)
		return 0
	}

	s, err := store.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watch: open db: %v\n", err)
		return 1
	}
	defer s.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	t := time.NewTicker(1 * time.Second)
	defer t.Stop()

	render := func() {
		out, err := renderSnapshot(s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "watch: render: %v\n", err)
			return
		}
		// Clear screen + home cursor, then print a wall-clock header (the only
		// place watch emits the current time) followed by the snapshot.
		fmt.Print("\033[H\033[2J")
		fmt.Printf("%s\n\n", time.Now().Format("Mon 15:04:05"))
		fmt.Print(out)
	}

	render()
	for {
		select {
		case <-ctx.Done():
			// Restore cursor visibility and exit cleanly.
			fmt.Print("\033[?25h")
			return 0
		case <-t.C:
			render()
		}
	}
}

// renderSnapshot reads every coordination table and returns a human-readable,
// column-aligned report of only the LIVE (non-expired) rows. It is factored out
// of Status/Watch so it can be unit tested deterministically (no wall-clock in
// the output).
func renderSnapshot(s *store.Store) (string, error) {
	now := store.Now()
	var b strings.Builder

	fmt.Fprintf(&b, "Airlock — %s\n\n", defaultAddr)

	if err := renderLocks(&b, s, now); err != nil {
		return "", err
	}
	if err := renderAgents(&b, s, now); err != nil {
		return "", err
	}
	if err := renderNotes(&b, s, now); err != nil {
		return "", err
	}
	if err := renderCounters(&b, s); err != nil {
		return "", err
	}
	if err := renderEvents(&b, s); err != nil {
		return "", err
	}
	if err := renderTasks(&b, s); err != nil {
		return "", err
	}

	return b.String(), nil
}

// renderLocks writes the LOCKS section: only locks with expires_at > now.
func renderLocks(b *strings.Builder, s *store.Store, now int64) error {
	rows, err := s.DB.Query(
		"SELECT name, agent_id, expires_at FROM locks WHERE expires_at > ? ORDER BY name",
		now)
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		name, heldBy string
		expiresAt    int64
	}
	var collected []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.heldBy, &r.expiresAt); err != nil {
			return err
		}
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	fmt.Fprintf(b, "LOCKS (%d)\n", len(collected))
	if len(collected) == 0 {
		fmt.Fprintln(b, "  (none)")
		fmt.Fprintln(b)
		return nil
	}
	tw := newTab(b)
	fmt.Fprintln(tw, "  NAME\tHELD BY\tEXPIRES IN")
	for _, r := range collected {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.name, r.heldBy, humanDur(int(r.expiresAt-now)))
	}
	tw.Flush()
	fmt.Fprintln(b)
	return nil
}

// renderAgents writes the AGENTS section: only agents with expires_at > now.
func renderAgents(b *strings.Builder, s *store.Store, now int64) error {
	rows, err := s.DB.Query(
		"SELECT agent_id, expires_at FROM agents WHERE expires_at > ? ORDER BY agent_id",
		now)
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		agent     string
		expiresAt int64
	}
	var collected []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.agent, &r.expiresAt); err != nil {
			return err
		}
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	fmt.Fprintf(b, "AGENTS (%d)\n", len(collected))
	if len(collected) == 0 {
		fmt.Fprintln(b, "  (none)")
		fmt.Fprintln(b)
		return nil
	}
	tw := newTab(b)
	fmt.Fprintln(tw, "  AGENT\tEXPIRES IN")
	for _, r := range collected {
		fmt.Fprintf(tw, "  %s\t%s\n", r.agent, humanDur(int(r.expiresAt-now)))
	}
	tw.Flush()
	fmt.Fprintln(b)
	return nil
}

// renderNotes writes the NOTES section: notes with no TTL or expires_at > now.
func renderNotes(b *strings.Builder, s *store.Store, now int64) error {
	rows, err := s.DB.Query(
		`SELECT key, value, author, expires_at FROM notes
		 WHERE expires_at IS NULL OR expires_at > ? ORDER BY key`,
		now)
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		key, value, author string
		expiresAt          *int64
	}
	var collected []row
	for rows.Next() {
		var r row
		var author *string
		if err := rows.Scan(&r.key, &r.value, &author, &r.expiresAt); err != nil {
			return err
		}
		if author != nil {
			r.author = *author
		}
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	fmt.Fprintf(b, "NOTES (%d)\n", len(collected))
	if len(collected) == 0 {
		fmt.Fprintln(b, "  (none)")
		fmt.Fprintln(b)
		return nil
	}
	tw := newTab(b)
	fmt.Fprintln(tw, "  KEY\tVALUE\tAUTHOR\tEXPIRES IN")
	for _, r := range collected {
		exp := "—"
		if r.expiresAt != nil {
			exp = humanDur(int(*r.expiresAt - now))
		}
		author := r.author
		if author == "" {
			author = "—"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.key, truncate(r.value, 40), author, exp)
	}
	tw.Flush()
	fmt.Fprintln(b)
	return nil
}

// renderCounters writes the COUNTERS section (counters never expire).
func renderCounters(b *strings.Builder, s *store.Store) error {
	rows, err := s.DB.Query("SELECT name, value FROM counters ORDER BY name")
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		name  string
		value int64
	}
	var collected []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.value); err != nil {
			return err
		}
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	fmt.Fprintf(b, "COUNTERS (%d)\n", len(collected))
	if len(collected) == 0 {
		fmt.Fprintln(b, "  (none)")
		fmt.Fprintln(b)
		return nil
	}
	tw := newTab(b)
	fmt.Fprintln(tw, "  NAME\tVALUE")
	for _, r := range collected {
		fmt.Fprintf(tw, "  %s\t%d\n", r.name, r.value)
	}
	tw.Flush()
	fmt.Fprintln(b)
	return nil
}

// renderEvents writes the EVENTS section (events never expire).
func renderEvents(b *strings.Builder, s *store.Store) error {
	rows, err := s.DB.Query("SELECT name, generation FROM events ORDER BY name")
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		name       string
		generation int64
	}
	var collected []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.generation); err != nil {
			return err
		}
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	fmt.Fprintf(b, "EVENTS (%d)\n", len(collected))
	if len(collected) == 0 {
		fmt.Fprintln(b, "  (none)")
		fmt.Fprintln(b)
		return nil
	}
	tw := newTab(b)
	fmt.Fprintln(tw, "  NAME\tGENERATION")
	for _, r := range collected {
		fmt.Fprintf(tw, "  %s\t%d\n", r.name, r.generation)
	}
	tw.Flush()
	fmt.Fprintln(b)
	return nil
}

// renderTasks writes the TASKS section. All non-done tasks are shown; the lease
// agent is rendered when present.
func renderTasks(b *strings.Builder, s *store.Store) error {
	rows, err := s.DB.Query(
		`SELECT id, queue, state, payload, lease_agent FROM tasks
		 WHERE state != 'done' ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		id                    int64
		queue, state, payload string
		leaseAgent            string
	}
	var collected []row
	for rows.Next() {
		var r row
		var lease *string
		if err := rows.Scan(&r.id, &r.queue, &r.state, &r.payload, &lease); err != nil {
			return err
		}
		if lease != nil {
			r.leaseAgent = *lease
		}
		collected = append(collected, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	fmt.Fprintf(b, "TASKS (%d)\n", len(collected))
	if len(collected) == 0 {
		fmt.Fprintln(b, "  (none)")
		fmt.Fprintln(b)
		return nil
	}
	tw := newTab(b)
	fmt.Fprintln(tw, "  ID\tQUEUE\tSTATE\tPAYLOAD\tLEASE AGENT")
	for _, r := range collected {
		lease := r.leaseAgent
		if lease == "" {
			lease = "—"
		}
		fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\n", r.id, r.queue, r.state, truncate(r.payload, 40), lease)
	}
	tw.Flush()
	fmt.Fprintln(b)
	return nil
}

// newTab returns a tabwriter writing into b with two-space-padded columns.
func newTab(b *strings.Builder) *tabwriter.Writer {
	return tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
}

// truncate shortens s to at most maxRunes runes, replacing the tail with an
// ellipsis when it overflows. It is rune-aware so multi-byte values are not
// split mid-character.
func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return "…"
	}
	return string(r[:maxRunes-1]) + "…"
}

// humanDur formats a duration given in seconds as a compact human string such
// as "1h2m", "45s", or "2d3h". Non-positive durations render as "0s".
func humanDur(seconds int) string {
	if seconds <= 0 {
		return "0s"
	}
	d := seconds / 86400
	h := (seconds % 86400) / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60

	switch {
	case d > 0:
		if h > 0 {
			return fmt.Sprintf("%dd%dh", d, h)
		}
		return fmt.Sprintf("%dd", d)
	case h > 0:
		if m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	case m > 0:
		if s > 0 {
			return fmt.Sprintf("%dm%ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
