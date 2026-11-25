package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const serviceName = "super-agent.service"

// systemdUnit renders the user service unit for super_agent daemon.
// The binary path is resolved to the current executable so the unit always
// points at the build you installed from. PATH is injected so that the
// worker spawn (which execs `pi`, a shell script needing node) works under
// systemd's minimal environment.
func systemdUnit(execPath string, pathEnv string) string {
	home, _ := os.UserHomeDir()
	return fmt.Sprintf(`[Unit]
Description=super_personal_agent daemon (agentd)
Documentation=file://%s/super_personal_agent/README.md
After=default.target

[Service]
Type=simple
ExecStart=%s daemon
Restart=on-failure
RestartSec=3
# Workers exec "pi" (a node script); systemd's default PATH lacks it.
Environment="PATH=%s"
# Give workers time to finish children before SIGKILL on stop
TimeoutStopSec=15
KillMode=mixed

[Install]
WantedBy=default.target
`, home, execPath, pathEnv)
}

// buildServicePATH constructs a PATH for the unit that includes the dirs
// containing `pi` and `node`, falling back to the current process PATH.
func buildServicePATH() string {
	seen := map[string]bool{}
	var dirs []string
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	// Prefer the dirs where pi and node actually live (stable paths)
	for _, tool := range []string{"pi", "node"} {
		if p, err := exec.LookPath(tool); err == nil {
			if rp, err := filepath.EvalSymlinks(p); err == nil {
				add(filepath.Dir(rp))
			}
			add(filepath.Dir(p))
		}
	}
	// Then the standard dirs as a base
	for _, d := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		add(d)
	}
	return strings.Join(dirs, ":")
}

// unitPath returns where the user unit file lives
func unitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", serviceName)
}

// cmdDaemonInstall writes the unit file and reloads systemd
func cmdDaemonInstall() error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	dir := filepath.Dir(unitPath())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir systemd user dir: %w", err)
	}

	content := systemdUnit(execPath, buildServicePATH())
	if err := os.WriteFile(unitPath(), []byte(content), 0644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}

	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %w\n%s", err, out)
	}

	fmt.Printf("✓ installed %s\n  ExecStart=%s daemon\n", unitPath(), execPath)
	fmt.Printf("  PATH=%s\n", buildServicePATH())
	fmt.Println("\nNext:")
	fmt.Println("  agent daemon start     # start now")
	fmt.Println("  agent daemon enable    # start on login")
	return nil
}

// systemctl runs a systemctl --user command and streams output
func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cmdDaemonStart starts the service
func cmdDaemonStart() error {
	if _, err := os.Stat(unitPath()); err != nil {
		fmt.Println("service not installed yet; installing...")
		if err := cmdDaemonInstall(); err != nil {
			return err
		}
	}
	if err := systemctl("start", serviceName); err != nil {
		return err
	}
	fmt.Println("✓ started. check: agent daemon status   logs: agent daemon logs")
	return nil
}

// cmdDaemonStop stops the service
func cmdDaemonStop() error {
	if err := systemctl("stop", serviceName); err != nil {
		return err
	}
	fmt.Println("✓ stopped")
	return nil
}

// cmdDaemonStatus shows service status
func cmdDaemonStatus() error {
	return systemctl("status", serviceName, "--no-pager", "-l")
}

// cmdDaemonLogs follows the journal for the service
func cmdDaemonLogs(follow bool) error {
	args := []string{"--user", "-u", serviceName, "--no-pager", "-n", "50"}
	if follow {
		args = append(args, "-f")
	}
	cmd := exec.Command("journalctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// cmdDaemonEnable enables start-on-login
func cmdDaemonEnable() error {
	if _, err := os.Stat(unitPath()); err != nil {
		if err := cmdDaemonInstall(); err != nil {
			return err
		}
	}
	if err := systemctl("enable", serviceName); err != nil {
		return err
	}
	fmt.Println("✓ enabled (starts on login)")
	fmt.Println("\nNote: to also run when logged OUT (no session), enable linger:")
	fmt.Println("  loginctl enable-linger $USER")
	return nil
}

// cmdDaemonDisable disables start-on-login
func cmdDaemonDisable() error {
	if err := systemctl("disable", serviceName); err != nil {
		return err
	}
	fmt.Println("✓ disabled")
	return nil
}

// cmdDaemonUninstall removes the unit file
func cmdDaemonUninstall() error {
	_ = systemctl("stop", serviceName)
	_ = systemctl("disable", serviceName)
	if err := os.Remove(unitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit: %w", err)
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %w\n%s", err, out)
	}
	fmt.Println("✓ uninstalled")
	return nil
}

// isSystemdAvailable checks systemctl --user works
func isSystemdAvailable() bool {
	err := exec.Command("systemctl", "--user", "status").Run()
	return err == nil
}

// daemonSubcommand dispatches `agent daemon <sub>`
func daemonSubcommand(sub string, rest []string) error {
	switch sub {
	case "", "run", "fg", "foreground":
		// Foreground mode (the original behavior)
		return cmdDaemon()
	case "install":
		return cmdDaemonInstall()
	case "start":
		return cmdDaemonStart()
	case "stop":
		return cmdDaemonStop()
	case "restart":
		if err := systemctl("restart", serviceName); err != nil {
			return err
		}
		fmt.Println("✓ restarted")
		return nil
	case "status":
		return cmdDaemonStatus()
	case "logs":
		follow := len(rest) > 0 && (rest[0] == "-f" || rest[0] == "--follow")
		return cmdDaemonLogs(follow)
	case "enable":
		return cmdDaemonEnable()
	case "disable":
		return cmdDaemonDisable()
	case "uninstall":
		return cmdDaemonUninstall()
	default:
		return fmt.Errorf("unknown daemon subcommand: %s\n\n%s", sub, daemonUsage())
	}
}

func daemonUsage() string {
	return strings.TrimSpace(`
agent daemon                 run in foreground (Ctrl+C to stop)
agent daemon install         install systemd user service
agent daemon start           start via systemd (background)
agent daemon stop            stop the service
agent daemon restart         restart the service
agent daemon status          show service status
agent daemon logs [-f]       show service logs (-f to follow)
agent daemon enable          start on login
agent daemon disable         don't start on login
agent daemon uninstall       remove the systemd service`)
}
