// Package svcinit registers the daemon as a systemd service (--init-enable)
// and removes that registration (--init-disable). Refs #35.
//
// Scope follows the invoking user: uid 0 installs a system-scope unit under
// /etc/systemd/system and enables it system-wide; any other uid installs a
// user-scope unit under ~/.config/systemd/user. Only units at those exact
// paths are managed — --init-disable refuses to touch a unit it did not
// write (e.g. one installed by the .deb package under /usr/lib/systemd).
package svcinit

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// UnitName is the systemd unit filename managed by this package.
const UnitName = "nextcloud-sync-daemon.service"

// Scope is the systemd scope a unit is installed into.
type Scope string

const (
	ScopeSystem Scope = "system"
	ScopeUser   Scope = "user"
)

// CurrentScope returns the scope for the invoking user: system for uid 0,
// user otherwise.
func CurrentScope() Scope {
	if os.Geteuid() == 0 {
		return ScopeSystem
	}
	return ScopeUser
}

// Options describes the unit to generate.
type Options struct {
	Scope      Scope
	BinaryPath string // absolute path written into ExecStart
	ConfigPath string // absolute path passed to --config
	LocalDir   string // sync directory, granted write access via ReadWritePaths
}

// Runner executes systemctl. Indirection for tests.
type Runner interface {
	Run(name string, args ...string) error
}

// ExecRunner runs commands for real, inheriting stdout/stderr so systemctl
// output reaches the user.
type ExecRunner struct{}

func (ExecRunner) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// UnitPath returns where the managed unit lives for the given scope.
// home is only used for user scope.
func UnitPath(scope Scope, home string) string {
	if scope == ScopeSystem {
		return filepath.Join("/etc/systemd/system", UnitName)
	}
	return filepath.Join(home, ".config", "systemd", "user", UnitName)
}

// UnitContent renders the unit file for the given options.
//
// The system-scope unit carries the same hardening as the packaged unit,
// with ReadWritePaths filled in from the validated config so the service
// starts without a manual drop-in. The user-scope unit omits the sandboxing
// directives: several of them (ProtectSystem, ProtectProc) need privileges
// or unprivileged user namespaces that are not uniformly available to user
// managers, and ProtectHome would block the common case of syncing a
// directory under $HOME.
func UnitContent(o Options) string {
	var b strings.Builder
	b.WriteString("# Installed by nextcloud-sync-daemon --init-enable (Refs #35).\n")
	b.WriteString("# Remove with nextcloud-sync-daemon --init-disable.\n")
	b.WriteString("\n[Unit]\n")
	b.WriteString("Description=Nextcloud Sync Daemon\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=notify\n")
	fmt.Fprintf(&b, "ExecStart=%s --config %s\n", o.BinaryPath, o.ConfigPath)
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("WatchdogSec=1800\n")
	if o.Scope == ScopeSystem {
		b.WriteString("\n# Security hardening (system scope)\n")
		b.WriteString("NoNewPrivileges=yes\n")
		b.WriteString("ProtectSystem=strict\n")
		fmt.Fprintf(&b, "ReadWritePaths=%s\n", o.LocalDir)
		b.WriteString("PrivateTmp=yes\n")
		b.WriteString("ProtectProc=invisible\n")
	}
	b.WriteString("\n[Install]\n")
	if o.Scope == ScopeSystem {
		b.WriteString("WantedBy=multi-user.target\n")
	} else {
		b.WriteString("WantedBy=default.target\n")
	}
	return b.String()
}

// systemctl returns the systemctl argument prefix for the scope.
func systemctlArgs(scope Scope, args ...string) []string {
	if scope == ScopeUser {
		return append([]string{"--user"}, args...)
	}
	return args
}

// Enable writes the unit file and enables + starts the service. It refuses
// to overwrite an existing unit at the managed path.
func Enable(o Options, home string, r Runner, out io.Writer) error {
	unitPath := UnitPath(o.Scope, home)

	if _, err := os.Stat(unitPath); err == nil {
		return fmt.Errorf("unit already exists at %s — run --init-disable first, or edit the existing unit", unitPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", unitPath, err)
	}

	if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
		return fmt.Errorf("creating unit directory: %w", err)
	}

	// O_EXCL closes the race between the Stat above and the write.
	f, err := os.OpenFile(unitPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("writing unit file: %w", err)
	}
	if _, err := f.WriteString(UnitContent(o)); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing unit file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing unit file: %w", err)
	}
	fmt.Fprintf(out, "wrote %s\n", unitPath)

	if err := r.Run("systemctl", systemctlArgs(o.Scope, "daemon-reload")...); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := r.Run("systemctl", systemctlArgs(o.Scope, "enable", "--now", UnitName)...); err != nil {
		return fmt.Errorf("systemctl enable --now: %w", err)
	}
	fmt.Fprintf(out, "enabled and started %s (%s scope)\n", UnitName, o.Scope)

	if o.Scope == ScopeUser {
		fmt.Fprintln(out, "note: user-scope services stop at logout unless lingering is on — run: loginctl enable-linger $USER")
	}
	return nil
}

// Disable stops + disables the service and removes the unit file, but only
// if the unit at the managed path was written by Enable (identified by the
// marker comment). A missing unit file is not an error — the disable calls
// still run so a half-removed state converges.
func Disable(scope Scope, home string, r Runner, out io.Writer) error {
	unitPath := UnitPath(scope, home)

	content, err := os.ReadFile(unitPath)
	unitExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", unitPath, err)
	}
	if unitExists && !strings.Contains(string(content), "--init-enable") {
		return fmt.Errorf("unit at %s was not installed by --init-enable — remove it yourself if that is what you want", unitPath)
	}

	if err := r.Run("systemctl", systemctlArgs(scope, "disable", "--now", UnitName)...); err != nil {
		// The unit may already be gone from systemd's view; report but continue
		// so the file still gets removed.
		fmt.Fprintf(out, "warning: systemctl disable --now failed: %v\n", err)
	}

	if unitExists {
		if err := os.Remove(unitPath); err != nil {
			return fmt.Errorf("removing unit file: %w", err)
		}
		fmt.Fprintf(out, "removed %s\n", unitPath)
	} else {
		fmt.Fprintf(out, "no unit file at %s\n", unitPath)
	}

	if err := r.Run("systemctl", systemctlArgs(scope, "daemon-reload")...); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	fmt.Fprintf(out, "disabled %s (%s scope)\n", UnitName, scope)
	return nil
}
