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
	"strconv"
	"strings"
)

// maxFindings bounds a scan. A tree that has gone badly wrong should not turn
// one warning into thousands, and the first few names are enough to act on.
const maxFindings = 50

// Result reports what a scan found.
type Result struct {
	// Paths are the offending paths, relative to the scanned root and escaped
	// for display. At most maxFindings entries.
	Paths []string
	// Total counts every offending path found, including those beyond
	// maxFindings.
	Total int
}

// Truncated reports whether Paths omits findings that Total counts.
func (r Result) Truncated() bool {
	return r.Total > len(r.Paths)
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

// Tree walks root and returns the paths whose names the server will reject.
//
// Walk errors on individual entries are skipped rather than aborting the scan:
// an unreadable subdirectory should not stop the daemon from reporting the
// names it can see. A failure to read the root itself is returned.
func Tree(root string) (Result, error) {
	var res Result

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
		if !HasControlChars(d.Name()) {
			return nil
		}

		res.Total++
		if len(res.Paths) < maxFindings {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			res.Paths = append(res.Paths, Escape(rel))
		}

		// A directory whose own name is unusable makes every path beneath it
		// unusable too. Report the directory and skip its contents rather than
		// listing every file inside it.
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}

	return res, nil
}

// Summary renders a Result for a log line.
func (r Result) Summary() string {
	if r.Total == 0 {
		return "none"
	}
	s := strings.Join(r.Paths, ", ")
	if r.Truncated() {
		s += ", …"
	}
	return s
}
