// Package scan finds local filenames a Nextcloud server will refuse.
//
// Linux filesystems accept almost any byte in a name; WebDAV does not. A file
// whose name holds a CR or LF cannot be expressed as a WebDAV path, so the
// server rejects the PUT with 400 and the sync client rolls that up to a failed
// sync. Every later sync then fails the same way, for as long as the file sits
// there: in #28 that was 248 consecutive failures, and nothing in the daemon's
// own output named the file (Refs #45).
package scan

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxFindings bounds what a scan keeps. A tree that has gone badly wrong should
// not grow the daemon's memory without limit; the count is exact well past the
// point where the individual names stop being useful.
const maxFindings = 10000

// maxSummaryNames bounds how many names a single log line carries. The first
// few are enough to act on, and the count says how many more there are.
const maxSummaryNames = 5

// Result reports what a scan found.
type Result struct {
	// Paths are the offending paths, relative to the scanned root and escaped
	// for display. At most maxFindings entries.
	Paths []string //nolint:godot
	// Total counts every offending path found, including those beyond
	// maxFindings.
	Total int
}

// Truncated reports whether Paths omits findings that Total counts.
func (r Result) Truncated() bool {
	return r.Total > len(r.Paths)
}

// conflictRe matches the names the sync client gives a conflicted copy:
//
//	report (conflicted copy 2026-04-10 191233).pdf
//	report (conflicted copy alice 2026-04-10 191233).pdf   (username variant)
//	report_conflict-20260410-191233.pdf                    (legacy ownCloud)
//
// These are ordinary files as far as the server is concerned. They sync
// happily, so nothing ever tells the user they exist (Refs #47).
var conflictRe = regexp.MustCompile(`(?i)\(conflicted copy[^)]*\)|_conflict-\d{8}-\d{6}`)

// IsConflictFile reports whether a filename is a conflicted copy left by the
// sync client.
func IsConflictFile(name string) bool {
	return conflictRe.MatchString(name)
}

// Conflict is a conflicted copy and when it was last written.
type Conflict struct {
	Path    string
	ModTime time.Time
}

// Findings is everything one walk of the tree turned up.
type Findings struct {
	// InvalidNames are paths the server will reject (Refs #45).
	InvalidNames Result
	// Conflicts are conflicted copies the client left behind, newest first
	// (Refs #47).
	Conflicts []Conflict
	// ConflictTotal counts every conflicted copy, including any beyond the
	// entries kept in Conflicts.
	ConflictTotal int
}

// HasControlChars reports whether a path holds a character the server cannot
// carry in a WebDAV path: the C0 controls (which include CR, LF and NUL) and
// DEL. Other awkward characters, such as backslash or colon, depend on the
// server's own configuration and are deliberately not judged here.
func HasControlChars(path string) bool {
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Escape renders a path safe to print. A name holding a real newline would
// otherwise split the log line that reports it, hiding the thing being
// reported.
func Escape(s string) string {
	if s == "" {
		return ""
	}
	if !HasControlChars(s) {
		return s
	}
	quoted := strconv.Quote(s)
	return quoted[1 : len(quoted)-1]
}

// Tree walks root once and returns everything worth reporting about it: names
// the server will reject, and conflicted copies the client has left behind.
//
// Walk errors on individual entries are skipped rather than aborting the scan:
// an unreadable subdirectory should not stop the daemon from reporting what it
// can see. A failure to read the root itself is returned.
func Tree(root string) (Findings, error) {
	var f Findings

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if path == root {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		if HasControlChars(d.Name()) {
			f.InvalidNames.Total++
			if len(f.InvalidNames.Paths) < maxFindings {
				f.InvalidNames.Paths = append(f.InvalidNames.Paths, Escape(rel))
			}

			// A directory whose own name is unusable makes every path beneath
			// it unusable too. Report the directory and skip its contents
			// rather than listing every file inside it.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if !d.IsDir() && IsConflictFile(d.Name()) {
			f.ConflictTotal++
			if len(f.Conflicts) < maxFindings {
				var mod time.Time
				if info, statErr := d.Info(); statErr == nil {
					mod = info.ModTime()
				}
				f.Conflicts = append(f.Conflicts, Conflict{Path: Escape(rel), ModTime: mod})
			}
		}
		return nil
	})
	if err != nil {
		return Findings{}, err
	}

	// Newest first: a conflict from this morning means something different
	// from one left three years ago.
	sort.SliceStable(f.Conflicts, func(i, j int) bool {
		return f.Conflicts[i].ModTime.After(f.Conflicts[j].ModTime)
	})

	return f, nil
}

// Summary renders a Result for a log line, naming at most maxSummaryNames
// paths. The full count travels alongside it as its own field.
func (r Result) Summary() string {
	if r.Total == 0 {
		return "none"
	}
	shown := r.Paths
	if len(shown) > maxSummaryNames {
		shown = shown[:maxSummaryNames]
	}
	s := strings.Join(shown, ", ")
	if r.Total > len(shown) {
		s += ", …"
	}
	return s
}

// maxSummaryConflicts bounds how many conflicted copies a log line names.
const maxSummaryConflicts = 3

// ConflictSummary renders the newest few conflicted copies for a log line.
func ConflictSummary(conflicts []Conflict) string {
	if len(conflicts) == 0 {
		return "none"
	}
	shown := conflicts
	if len(shown) > maxSummaryConflicts {
		shown = shown[:maxSummaryConflicts]
	}
	names := make([]string, 0, len(shown))
	for _, c := range shown {
		names = append(names, c.Path)
	}
	s := strings.Join(names, ", ")
	if len(conflicts) > len(shown) {
		s += ", …"
	}
	return s
}
