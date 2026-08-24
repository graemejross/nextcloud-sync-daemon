package scan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	res, err := Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("Total = %d, want 1", res.Total)
	}
	want := `sub/Schematic\r\n (As-Is)v2.pdf`
	if len(res.Paths) != 1 || res.Paths[0] != want {
		t.Errorf("Paths = %q, want [%q]", res.Paths, want)
	}
	if res.Truncated() {
		t.Error("Truncated() = true for a single finding")
	}
}

func TestTreeCleanTree(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "fine.txt"))

	res, err := Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if res.Total != 0 || len(res.Paths) != 0 {
		t.Errorf("clean tree reported %+v", res)
	}
	if res.Summary() != "none" {
		t.Errorf("Summary = %q, want none", res.Summary())
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

	res, err := Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if res.Total != 1 {
		t.Errorf("Total = %d, want 1 (the directory, not its contents)", res.Total)
	}
	if len(res.Paths) != 1 || !strings.Contains(res.Paths[0], `album\r2026`) {
		t.Errorf("Paths = %q, want the directory", res.Paths)
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

	res, err := Tree(root)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if res.Total != count {
		t.Errorf("Total = %d, want %d", res.Total, count)
	}
	if len(res.Paths) != count {
		t.Errorf("Paths holds %d entries, want all %d", len(res.Paths), count)
	}

	summary := res.Summary()
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
