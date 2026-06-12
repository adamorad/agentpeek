// Package cli implements the human-facing subcommands (status, watch) that talk
// to a running airlock daemon and render its state to the terminal.
package cli

import "fmt"

// Status prints a one-shot snapshot of currently tracked/reserved ports.
//
// Placeholder: a future task will connect to the running daemon (or read the
// shared state.db), then render the active port table.
func Status() int {
	fmt.Println("status: not yet implemented")
	return 0
}

// Watch streams live port-activity updates until interrupted.
//
// Placeholder: a future task will subscribe to daemon events and continuously
// re-render the active port table.
func Watch() int {
	fmt.Println("watch: not yet implemented")
	return 0
}
