package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHasControlChars(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"clean", "Documents/report.pdf", false},
		{"spaces and unicode", "Photos/été 2026/plan (final).pdf", false},
		{"carriage return", "Schematic\r\n (As-Is)v2.pdf", true},
		{"newline only", "notes\ndraft.md", true},
		{"tab", "odd\tname.txt", true},
		{"nul", "broken\x00.txt", true},
		{"delete", "odd\x7f.txt", true},
		{"backslash is not a control character", `weird\name.txt`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasControlChars(tt.path); got != tt.want {
				t.Errorf("HasControlChars(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestEscape(t *testing.T) {
	got := Escape("Schematic\r\n v2.pdf")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("Escape left a raw control character: %q", got)
	}
	if got != `Schematic\r\n v2.pdf` {
		t.Errorf("Escape = %q, want the escaped form", got)
	}
	if Escape("clean/path.txt") != "clean/path.txt" {
		t.Error("Escape altered a clean path")
	}
}

// writeFile creates a file, tolerating names the test itself cares about.
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing %q: %v", path, err)
	}
}

func TestTreeFindsOffendingNames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "fine.txt"))
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "sub", "also-fine.md"))
	writeFile(t, filepath.Join(root, "sub", "Schematic\r\n (As-Is)v2.pdf"))

	found, err := Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if found.InvalidNames.Total != 1 {
		t.Fatalf("Total = %d, want 1", found.InvalidNames.Total)
	}
	want := `sub/Schematic\r\n (As-Is)v2.pdf`
	if len(found.InvalidNames.Paths) != 1 || found.InvalidNames.Paths[0] != want {
		t.Errorf("Paths = %q, want [%q]", found.InvalidNames.Paths, want)
	}
	if found.InvalidNames.Truncated() {
		t.Error("Truncated() = true for a single finding")
	}
}

func TestTreeCleanTree(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "fine.txt"))

	found, err := Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if found.InvalidNames.Total != 0 || len(found.InvalidNames.Paths) != 0 {
		t.Errorf("clean tree reported %+v", found)
	}
	if found.InvalidNames.Summary() != "none" {
		t.Errorf("Summary = %q, want none", found.InvalidNames.Summary())
	}
}

// A directory with an unusable name makes every path under it unusable. Report
// the directory once instead of every file inside it.
func TestTreeReportsBadDirectoryOnce(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "album\r2026")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(bad, "one.jpg"))
	writeFile(t, filepath.Join(bad, "two.jpg"))

	found, err := Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if found.InvalidNames.Total != 1 {
		t.Errorf("Total = %d, want 1 (the directory, not its contents)", found.InvalidNames.Total)
	}
	if len(found.InvalidNames.Paths) != 1 || !strings.Contains(found.InvalidNames.Paths[0], `album\r2026`) {
		t.Errorf("Paths = %q, want the directory", found.InvalidNames.Paths)
	}
}

// A scan keeps every finding it meets (up to a hard memory limit), but a log
// line names only the first few and says there are more.
func TestSummaryNamesAFewAndMarksTheRest(t *testing.T) {
	root := t.TempDir()
	const count = maxSummaryNames + 3
	for i := 0; i < count; i++ {
		writeFile(t, filepath.Join(root, "bad\r"+string(rune('a'+i))+".txt"))
	}

	found, err := Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if found.InvalidNames.Total != count {
		t.Errorf("Total = %d, want %d", found.InvalidNames.Total, count)
	}
	if len(found.InvalidNames.Paths) != count {
		t.Errorf("Paths holds %d entries, want all %d", len(found.InvalidNames.Paths), count)
	}

	summary := found.InvalidNames.Summary()
	if n := strings.Count(summary, ".txt"); n != maxSummaryNames {
		t.Errorf("summary named %d paths, want %d", n, maxSummaryNames)
	}
	if !strings.HasSuffix(summary, "…") {
		t.Errorf("summary did not mark the omitted paths: %q", summary)
	}
}

func TestTreeMissingRoot(t *testing.T) {
	if _, err := Tree(filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
		t.Error("expected an error for a missing root")
	}
}

func TestIsConflictFile(t *testing.T) {
	tests := []struct {
		name string
		file string
		want bool
	}{
		{"current client format", "report (conflicted copy 2026-04-10 191233).pdf", true},
		{"username variant", "report (conflicted copy alice 2026-04-10 191233).pdf", true},
		{"legacy ownCloud", "report_conflict-20260410-191233.pdf", true},
		{"different case", "report (Conflicted Copy 2026-04-10 191233).pdf", true},
		{"ordinary file", "report.pdf", false},
		{"mentions conflict in prose", "conflict resolution notes.md", false},
		{"parenthesised but not a conflict", "report (final draft).pdf", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsConflictFile(tt.file); got != tt.want {
				t.Errorf("IsConflictFile(%q) = %v, want %v", tt.file, got, tt.want)
			}
		})
	}
}

func TestTreeFindsConflictFilesNewestFirst(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "report.pdf"))

	old := filepath.Join(root, "report (conflicted copy 2023-01-02 101010).pdf")
	recent := filepath.Join(root, "report (conflicted copy 2026-04-10 191233).pdf")
	writeFile(t, old)
	writeFile(t, recent)

	oldTime := time.Date(2023, 1, 2, 10, 10, 10, 0, time.UTC)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	found, err := Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if found.ConflictTotal != 2 {
		t.Fatalf("ConflictTotal = %d, want 2", found.ConflictTotal)
	}
	if len(found.Conflicts) != 2 {
		t.Fatalf("Conflicts holds %d entries, want 2", len(found.Conflicts))
	}
	if !strings.Contains(found.Conflicts[0].Path, "2026-04-10") {
		t.Errorf("newest conflict is %q, want the 2026 one first", found.Conflicts[0].Path)
	}
	if found.Conflicts[1].ModTime.Year() != 2023 {
		t.Errorf("second entry has mtime %v, want the 2023 one", found.Conflicts[1].ModTime)
	}
	// The conflicted copies are not invalid names.
	if found.InvalidNames.Total != 0 {
		t.Errorf("InvalidNames.Total = %d, want 0", found.InvalidNames.Total)
	}
}

func TestConflictSummary(t *testing.T) {
	if got := ConflictSummary(nil); got != "none" {
		t.Errorf("ConflictSummary(nil) = %q, want none", got)
	}

	var conflicts []Conflict
	for i := 0; i < maxSummaryConflicts+2; i++ {
		conflicts = append(conflicts, Conflict{Path: fmt.Sprintf("file-%d.pdf", i)})
	}
	got := ConflictSummary(conflicts)
	if n := strings.Count(got, ".pdf"); n != maxSummaryConflicts {
		t.Errorf("summary named %d paths, want %d", n, maxSummaryConflicts)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("summary did not mark the omitted entries: %q", got)
	}
}
