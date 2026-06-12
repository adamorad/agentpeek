// Package service installs and removes the airlock background daemon as a
// platform-native service: a launchd LaunchAgent on macOS, a systemd user unit
// on Linux. The file-generation functions (LaunchdPlist, SystemdUnit) are pure
// and unit-testable; Install/Uninstall shell out to launchctl/systemctl.
//
// Setting AIRLOCK_SERVICE_DRYRUN=1 makes Install/Uninstall print the file they
// would write and the commands they would run, without touching the filesystem
// or executing anything — useful for testing the command path safely.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// launchdLabel is the launchd label for the v2 daemon. It deliberately matches
// the renamed Swift v1 label so loading the new agent supersedes it on the same
// port. The pre-rename v1 label (agentpeekLabel) is also handled on takeover.
const launchdLabel = "com.airlock.daemon"

// agentpeekLabel is the pre-rename v1 launchd label. A user upgrading from the
// earliest v1 may still have this agent loaded; Install unloads it too so the
// new daemon can claim the port cleanly.
const agentpeekLabel = "com.agentpeek.daemon"

// systemdUnitName is the systemd user unit filename for the Linux daemon.
const systemdUnitName = "airlock.service"

// fallbackBinPath is used when os.Executable cannot resolve the running binary.
const fallbackBinPath = "/usr/local/bin/airlock"

// dryRunEnv, when set to "1", switches Install/Uninstall into print-only mode.
const dryRunEnv = "AIRLOCK_SERVICE_DRYRUN"

// LaunchdPlist returns the launchd LaunchAgent plist XML for the airlock daemon
// running binPath. The agent runs at load, is kept alive, and logs stdout and
// stderr to /tmp/airlock.log.
func LaunchdPlist(binPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardErrorPath</key>
	<string>/tmp/airlock.log</string>
	<key>StandardOutPath</key>
	<string>/tmp/airlock.log</string>
</dict>
</plist>
`, launchdLabel, binPath)
}

// SystemdUnit returns the systemd user unit for the airlock daemon running
// binPath. The service restarts automatically and is wanted by the default
// user target so it starts on login.
func SystemdUnit(binPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Airlock agent coordination daemon

[Service]
ExecStart=%s daemon
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`, binPath)
}

// Install detects the host OS and installs airlock as a background service. On
// darwin it writes a launchd LaunchAgent and (re)loads it, performing a clean
// takeover of any prior v1 agent. On linux it writes a systemd user unit and
// enables it. Other platforms return an error directing the user to run the
// daemon manually.
func Install() error {
	switch runtime.GOOS {
	case "darwin":
		return installDarwin()
	case "linux":
		return installLinux()
	default:
		return fmt.Errorf("service install not supported on %s; run `airlock daemon` manually", runtime.GOOS)
	}
}

// Uninstall removes the airlock background service for the host OS. It is
// best-effort and idempotent: a missing unit or already-unloaded agent is not
// an error.
func Uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallDarwin()
	case "linux":
		return uninstallLinux()
	default:
		return fmt.Errorf("service uninstall not supported on %s", runtime.GOOS)
	}
}

// binaryPath returns the path of the running binary via os.Executable, falling
// back to a conventional install location if that fails.
func binaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return fallbackBinPath
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}

// dryRun reports whether print-only mode is enabled via the environment.
func dryRun() bool {
	return os.Getenv(dryRunEnv) == "1"
}

// installDarwin writes the launchd plist and loads it, after unloading any
// prior v1 agents so the new daemon can claim the port.
func installDarwin() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	agentsDir := filepath.Join(home, "Library", "LaunchAgents")
	plistPath := filepath.Join(agentsDir, launchdLabel+".plist")
	oldPlistPath := filepath.Join(agentsDir, agentpeekLabel+".plist")

	bin := binaryPath()
	plist := LaunchdPlist(bin)

	if dryRun() {
		fmt.Printf("[dry-run] would write %s (0644):\n%s\n", plistPath, plist)
		fmt.Printf("[dry-run] would run: launchctl unload %s\n", oldPlistPath)
		fmt.Printf("[dry-run] would run: launchctl unload %s\n", plistPath)
		fmt.Printf("[dry-run] would run: launchctl load %s\n", plistPath)
		return nil
	}

	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	// Port handover: best-effort unload of any prior v1 agents (pre-rename and
	// renamed) before loading the new one. Unloading a non-loaded agent is not
	// fatal, so errors here are ignored.
	_ = runCmd("launchctl", "unload", oldPlistPath)
	_ = runCmd("launchctl", "unload", plistPath)

	if err := runCmd("launchctl", "load", plistPath); err != nil {
		return fmt.Errorf("launchctl load %s: %w", plistPath, err)
	}

	fmt.Printf("Installed launchd agent: %s\n", plistPath)
	fmt.Println("airlock daemon is now running and will start at login.")
	fmt.Println("Check it with: airlock status")
	return nil
}

// uninstallDarwin unloads and removes the launchd plist. Both steps are
// best-effort so the function is idempotent.
func uninstallDarwin() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")

	if dryRun() {
		fmt.Printf("[dry-run] would run: launchctl unload %s\n", plistPath)
		fmt.Printf("[dry-run] would remove %s\n", plistPath)
		return nil
	}

	_ = runCmd("launchctl", "unload", plistPath)
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}
	fmt.Printf("Removed launchd agent: %s\n", plistPath)
	return nil
}

// installLinux writes the systemd user unit and enables it. If systemctl is not
// available, it still writes the unit and prints manual start instructions.
func installLinux() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	unitPath := filepath.Join(unitDir, systemdUnitName)

	bin := binaryPath()
	unit := SystemdUnit(bin)

	if dryRun() {
		fmt.Printf("[dry-run] would write %s (0644):\n%s\n", unitPath, unit)
		fmt.Println("[dry-run] would run: systemctl --user daemon-reload")
		fmt.Printf("[dry-run] would run: systemctl --user enable --now %s\n", systemdUnitName)
		return nil
	}

	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("create systemd user dir: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}

	// If systemctl is missing, the unit is written but cannot be activated by us.
	if _, err := exec.LookPath("systemctl"); err != nil {
		fmt.Printf("Wrote systemd unit: %s\n", unitPath)
		fmt.Println("systemctl was not found, so the service was not started automatically.")
		fmt.Println("Start the daemon yourself with: airlock daemon")
		return nil
	}

	if err := runCmd("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := runCmd("systemctl", "--user", "enable", "--now", systemdUnitName); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %w", systemdUnitName, err)
	}

	fmt.Printf("Installed systemd user unit: %s\n", unitPath)
	fmt.Println("airlock daemon is now running and will start on login.")
	fmt.Println("Check it with: airlock status")
	return nil
}

// uninstallLinux disables the systemd user unit and removes it. Both steps are
// best-effort so the function is idempotent.
func uninstallLinux() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdUnitName)

	if dryRun() {
		fmt.Printf("[dry-run] would run: systemctl --user disable --now %s\n", systemdUnitName)
		fmt.Printf("[dry-run] would remove %s\n", unitPath)
		return nil
	}

	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = runCmd("systemctl", "--user", "disable", "--now", systemdUnitName)
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit: %w", err)
	}
	fmt.Printf("Removed systemd user unit: %s\n", unitPath)
	return nil
}

// runCmd executes name with args, attaching the child's stderr to a buffer so a
// failure can surface the underlying message. stdout/stderr are otherwise
// discarded since these tools are quiet on success.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
