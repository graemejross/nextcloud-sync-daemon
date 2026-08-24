package sync

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxDetailLen bounds the error detail we carry into logs and the health
// endpoint. nextcloudcmd error strings are normally short, but a server can
// return an HTML error body, and an unbounded field would land whole in the
// journal.
const maxDetailLen = 512

// failure describes the item that killed a sync, recovered from nextcloudcmd's
// stderr (Refs #44).
//
// nextcloudcmd reports every per-file outcome at the same visual weight, so a
// BlacklistedError looks exactly like a Conflict or a FileIgnored. Only the
// status token separates the item that failed the sync from the ones that were
// merely skipped, and the run's exit code carries none of it.
type failure struct {
	// Reason is the SyncFileItem::Status token, e.g. "BlacklistedError".
	Reason string
	// Path is the item's path relative to the sync root. Empty if the line
	// carried no path.
	Path string
	// Detail is the client's error string for the item, or the whole matched
	// line when no error string was parsed.
	Detail string
}

// found reports whether anything was recovered from stderr.
func (f failure) found() bool {
	return f.Reason != "" || f.Path != "" || f.Detail != ""
}

// errorStatuses are the SyncFileItem::Status values that mean an item failed,
// as opposed to being skipped or resolved. Taken from the enum in
// libnextcloudsync; Success, Conflict, FileIgnored, Restoration, Excluded and
// NoStatus are deliberately absent, since a sync that ends non-zero does so
// because of the statuses below.
var errorStatuses = []string{
	"FatalError",
	"BlacklistedError",
	"FileNameInvalidOnServer",
	"FileNameInvalid",
	"FileNameClash",
	"FileLocked",
	"NormalError",
	"DetailError",
	"SoftError",
}

// isErrorStatus reports whether a SyncFileItem::Status token means the item
// failed. Success, Conflict, FileIgnored and the rest are outcomes the sync
// survives, and reporting one as the cause would point the reader at an
// innocent file.
func isErrorStatus(status string) bool {
	for _, s := range errorStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// propagationRe matches the propagator's terminal line for a failed item:
//
//	Could not complete propagation of "<path>" by <job> with status <Status> and error: "<detail>"
//
// The path and detail are Qt-quoted, so control characters inside a filename
// arrive escaped (\r, \n) and the line stays a single line — which is how the
// CR+LF filename behind #28 could have been read straight out of the log.
var propagationRe = regexp.MustCompile(
	`Could not complete propagation of "((?:[^"\\]|\\.)*)".*?with status ([A-Za-z]+)(?:\s+and error:\s+"((?:[^"\\]|\\.)*)")?`,
)

// statusRe matches a bare status token anywhere in a line, for client versions
// whose wording differs from propagationRe. Longer tokens lead so that
// FileNameInvalidOnServer is not truncated to FileNameInvalid.
var statusRe = regexp.MustCompile(`\b(` + strings.Join(errorStatuses, "|") + `)\b`)

// quotedRe matches a Qt-quoted string, used to recover a path from a line that
// propagationRe did not match.
var quotedRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// parseFailure recovers the failing item from captured stderr.
//
// It prefers the first propagator line, which names the item, its status and
// the server's error. Propagation rolls failures up to the root directory, so
// the earliest item error is the cause and the later lines are its echo. When
// no propagator line matches, it falls back to the first line carrying an error
// status token, and finally to the last non-empty line, which at least gives
// the reader somewhere to start.
func parseFailure(stderr string) failure {
	if strings.TrimSpace(stderr) == "" {
		return failure{}
	}

	lines := strings.Split(stderr, "\n")

	// Preferred: the propagator's own report of the failing item. The same
	// line shape reports successes and conflicts, so the status decides
	// whether it is a failure at all.
	for _, line := range lines {
		m := propagationRe.FindStringSubmatch(line)
		if m == nil || !isErrorStatus(m[2]) {
			continue
		}
		return failure{
			Reason: m[2],
			Path:   sanitize(m[1]),
			Detail: truncate(sanitize(m[3])),
		}
	}

	// Fallback: any line carrying an error status token.
	for _, line := range lines {
		m := statusRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		return failure{
			Reason: m[1],
			Path:   pathFromLine(line),
			Detail: truncate(sanitize(strings.TrimSpace(line))),
		}
	}

	// Last resort: the final line of output.
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return failure{Detail: truncate(sanitize(line))}
		}
	}

	return failure{}
}

// pathFromLine picks the most path-like quoted string on a line. A quoted
// string counts as a path if it contains a separator or a dot, which keeps
// "400 Bad Request" and similar server messages out of the path field.
func pathFromLine(line string) string {
	for _, m := range quotedRe.FindAllStringSubmatch(line, -1) {
		candidate := m[1]
		if candidate == "" {
			continue
		}
		if strings.ContainsAny(candidate, "/\\") || strings.Contains(candidate, ".") {
			return sanitize(candidate)
		}
	}
	return ""
}

// sanitize makes a value safe to put in a log line or a JSON field. Filenames
// can legally hold control characters — a CR+LF filename is what broke the sync
// in #28 — and writing one raw would split the log line and hide the very thing
// being reported.
func sanitize(s string) string {
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			quoted := strconv.Quote(s)
			return quoted[1 : len(quoted)-1]
		}
	}
	return s
}

// truncate bounds a field, marking any value it shortens. It cuts on a rune
// boundary so a shortened value stays valid UTF-8 in the JSON health response.
func truncate(s string) string {
	if len(s) <= maxDetailLen {
		return s
	}
	cut := s[:maxDetailLen]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…(truncated)"
}
