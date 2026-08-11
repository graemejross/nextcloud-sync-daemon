package svcinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records systemctl invocations.
type fakeRunner struct {
	calls [][]string
	err   error
}

func (f *fakeRunner) Run(name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.err
}

func testOptions(scope Scope) Options {
	return Options{
		Scope:      scope,
		BinaryPath: "/usr/local/bin/nextcloud-sync-daemon",
		ConfigPath: "/etc/nextcloud-sync-daemon/config.yaml",
		LocalDir:   "/srv/nextcloud-sync",
	}
}

func TestUnitContentSystem(t *testing.T) {
	c := UnitContent(testOptions(ScopeSystem))

	for _, want := range []string{
		"ExecStart=/usr/local/bin/nextcloud-sync-daemon --config /etc/nextcloud-sync-daemon/config.yaml",
		"Type=notify",
		"ProtectSystem=strict",
		"ReadWritePaths=/srv/nextcloud-sync",
		"ProtectProc=invisible",
		"WantedBy=multi-user.target",
		"--init-enable", // marker Disable checks for
	} {
		if !strings.Contains(c, want) {
			t.Errorf("system unit missing %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "ProtectHome") {
		t.Error("system unit must not set ProtectHome — the sync dir may live under /home")
	}
}

func TestUnitContentUser(t *testing.T) {
	c := UnitContent(testOptions(ScopeUser))

	for _, want := range []string{
		"ExecStart=/usr/local/bin/nextcloud-sync-daemon --config /etc/nextcloud-sync-daemon/config.yaml",
		"WantedBy=default.target",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("user unit missing %q:\n%s", want, c)
		}
	}
	// Sandboxing directives are system-scope only.
	for _, banned := range []string{"ProtectSystem", "ReadWritePaths", "ProtectProc", "PrivateTmp"} {
		if strings.Contains(c, banned) {
			t.Errorf("user unit must not contain %q:\n%s", banned, c)
		}
	}
}

func TestUnitPath(t *testing.T) {
	if got := UnitPath(ScopeSystem, "/home/x"); got != "/etc/systemd/system/nextcloud-sync-daemon.service" {
		t.Errorf("system path = %s", got)
	}
	if got := UnitPath(ScopeUser, "/home/x"); got != "/home/x/.config/systemd/user/nextcloud-sync-daemon.service" {
		t.Errorf("user path = %s", got)
	}
}

func TestEnableWritesUnitAndRunsSystemctl(t *testing.T) {
	home := t.TempDir()
	r := &fakeRunner{}
	var out strings.Builder

	if err := Enable(testOptions(ScopeUser), home, r, &out); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	unit := UnitPath(ScopeUser, home)
	data, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if !strings.Contains(string(data), "ExecStart=") {
		t.Error("unit content missing ExecStart")
	}

	want := [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", UnitName},
	}
	if len(r.calls) != len(want) {
		t.Fatalf("systemctl calls = %v", r.calls)
	}
	for i := range want {
		if strings.Join(r.calls[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, r.calls[i], want[i])
		}
	}
	if !strings.Contains(out.String(), "enable-linger") {
		t.Error("user-scope enable should mention loginctl enable-linger")
	}
}

func TestEnableSystemScopeCallsPlainSystemctl(t *testing.T) {
	// System scope must not pass --user. Unit path is under /etc, so run
	// Enable against a home dir anyway and intercept before the file write
	// by pre-creating the path? Not possible without root — so test the
	// argument builder directly.
	got := systemctlArgs(ScopeSystem, "enable", "--now", UnitName)
	if strings.Join(got, " ") != "enable --now "+UnitName {
		t.Errorf("system args = %v", got)
	}
	gotUser := systemctlArgs(ScopeUser, "daemon-reload")
	if strings.Join(gotUser, " ") != "--user daemon-reload" {
		t.Errorf("user args = %v", gotUser)
	}
}

func TestEnableRefusesOverwrite(t *testing.T) {
	home := t.TempDir()
	unit := UnitPath(ScopeUser, home)
	if err := os.MkdirAll(filepath.Dir(unit), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &fakeRunner{}
	err := Enable(testOptions(ScopeUser), home, r, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists error, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("systemctl must not run when refusing: %v", r.calls)
	}
	data, _ := os.ReadFile(unit)
	if string(data) != "existing" {
		t.Error("existing unit was modified")
	}
}

func TestDisableRemovesManagedUnit(t *testing.T) {
	home := t.TempDir()
	r := &fakeRunner{}
	if err := Enable(testOptions(ScopeUser), home, r, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	r.calls = nil

	if err := Disable(ScopeUser, home, r, &strings.Builder{}); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := os.Stat(UnitPath(ScopeUser, home)); !os.IsNotExist(err) {
		t.Error("unit file not removed")
	}

	want := [][]string{
		{"systemctl", "--user", "disable", "--now", UnitName},
		{"systemctl", "--user", "daemon-reload"},
	}
	for i := range want {
		if strings.Join(r.calls[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, r.calls[i], want[i])
		}
	}
}

func TestDisableRefusesForeignUnit(t *testing.T) {
	home := t.TempDir()
	unit := UnitPath(ScopeUser, home)
	if err := os.MkdirAll(filepath.Dir(unit), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Unit]\nDescription=someone else's\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &fakeRunner{}
	err := Disable(ScopeUser, home, r, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "not installed by --init-enable") {
		t.Fatalf("expected foreign-unit refusal, got %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("systemctl must not run when refusing: %v", r.calls)
	}
	if _, statErr := os.Stat(unit); statErr != nil {
		t.Error("foreign unit must not be removed")
	}
}

func TestDisableMissingUnitStillConverges(t *testing.T) {
	home := t.TempDir()
	r := &fakeRunner{}
	var out strings.Builder

	if err := Disable(ScopeUser, home, r, &out); err != nil {
		t.Fatalf("Disable with no unit: %v", err)
	}
	if len(r.calls) != 2 {
		t.Errorf("expected disable + daemon-reload to still run: %v", r.calls)
	}
	if !strings.Contains(out.String(), "no unit file") {
		t.Errorf("output should say no unit file: %s", out.String())
	}
}
