package sync

import (
	"strings"
	"testing"
)

// Sample lines follow the shape nextcloudcmd 3.11 writes to stderr: a Qt
// categorised-logging prefix, then the message. Paths and error strings arrive
// Qt-quoted, which is what keeps an escaped control character on one line.
const (
	noiseLines = `2026-04-10 19:12:31:001 [ info nextcloud.sync.engine ]:	#### Discovery start ####
2026-04-10 19:12:32:114 [ warning nextcloud.sync.propagator ]:	Could not complete propagation of "Reports/last-year.csv" by OCC::PropagateDownloadFile(0x5581) with status Conflict and error: "conflict"
2026-04-10 19:12:32:220 [ info nextcloud.sync.propagator ]:	Completed propagation of "Notes/todo.md" by OCC::PropagateUploadFileNG(0x5582) with status Success
`

	blacklistedLine = `2026-04-10 19:12:33:441 [ warning nextcloud.sync.propagator ]:	Could not complete propagation of "Drawings/Schematic\r\n (As-Is)v2.pdf" by OCC::PropagateUploadFileNG(0x5583) with status BlacklistedError and error: "400 Bad Request"
`

	rollupLines = `2026-04-10 19:12:34:002 [ warning nextcloud.sync.propagator ]:	PropagateRootDirectory::slotDirDeletionJobsFinished reporting previous error
2026-04-10 19:12:34:003 [ info nextcloud.sync.engine ]:	SyncEngine::finalize modify final status NormalError
`
)

func TestParseFailurePrefersPropagatorLine(t *testing.T) {
	f := parseFailure(noiseLines + blacklistedLine + rollupLines)

	if f.Reason != "BlacklistedError" {
		t.Errorf("Reason = %q, want BlacklistedError", f.Reason)
	}
	// The CR+LF in the filename must survive as an escape, not as a real
	// newline that would split the log line (Refs #28).
	want := `Drawings/Schematic\r\n (As-Is)v2.pdf`
	if f.Path != want {
		t.Errorf("Path = %q, want %q", f.Path, want)
	}
	if f.Detail != "400 Bad Request" {
		t.Errorf("Detail = %q, want 400 Bad Request", f.Detail)
	}
}

func TestParseFailureSkipsNonErrorStatuses(t *testing.T) {
	// Conflict and Success are outcomes, not failures: a run containing only
	// those and a rollup line must not report the conflicting file as the cause.
	f := parseFailure(noiseLines + rollupLines)

	if f.Path == "Reports/last-year.csv" {
		t.Errorf("reported a Conflict item as the failure: %+v", f)
	}
	if f.Reason != "NormalError" {
		t.Errorf("Reason = %q, want NormalError from the rollup line", f.Reason)
	}
}

func TestParseFailureFallsBackToStatusToken(t *testing.T) {
	// A client whose wording differs from the 3.11 propagator line still
	// carries the status token and a quoted path.
	in := `2026-04-10 19:12:33:441 [ warning nextcloud.sync.propagator ]:	item "Photos/holiday.heic" finished, status FileNameInvalidOnServer, msg "Filename contains invalid characters"
`
	f := parseFailure(in)

	if f.Reason != "FileNameInvalidOnServer" {
		t.Errorf("Reason = %q, want FileNameInvalidOnServer", f.Reason)
	}
	if f.Path != "Photos/holiday.heic" {
		t.Errorf("Path = %q, want Photos/holiday.heic", f.Path)
	}
	if !strings.Contains(f.Detail, "invalid characters") {
		t.Errorf("Detail = %q, want the whole line", f.Detail)
	}
}

func TestParseFailureFallsBackToLastLine(t *testing.T) {
	in := "connecting...\nnextcloudcmd: server replied 503\n\n"
	f := parseFailure(in)

	if f.Reason != "" {
		t.Errorf("Reason = %q, want empty when no status token is present", f.Reason)
	}
	if f.Detail != "nextcloudcmd: server replied 503" {
		t.Errorf("Detail = %q, want the last non-empty line", f.Detail)
	}
	if !f.found() {
		t.Error("found() = false, want true when a detail was recovered")
	}
}

func TestParseFailureEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n"} {
		f := parseFailure(in)
		if f.found() {
			t.Errorf("parseFailure(%q) = %+v, want nothing found", in, f)
		}
	}
}

func TestPathFromLineIgnoresServerMessages(t *testing.T) {
	// "400 Bad Request" is quoted too; it must not end up in the path field.
	got := pathFromLine(`status BlacklistedError and error: "400 Bad Request"`)
	if got != "" {
		t.Errorf("pathFromLine = %q, want empty", got)
	}
}

func TestSanitizeEscapesControlCharacters(t *testing.T) {
	got := sanitize("Schematic\r\n v2.pdf")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("sanitize left a raw control character: %q", got)
	}
	if got != `Schematic\r\n v2.pdf` {
		t.Errorf("sanitize = %q, want escaped form", got)
	}
	if sanitize("plain/path.txt") != "plain/path.txt" {
		t.Error("sanitize altered a clean path")
	}
}

func TestTruncateBoundsDetail(t *testing.T) {
	long := strings.Repeat("x", maxDetailLen+50)
	got := truncate(long)
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("truncate did not mark the cut: %q", got[len(got)-20:])
	}
	if len(got) > maxDetailLen+len("…(truncated)") {
		t.Errorf("truncate returned %d bytes, want at most %d", len(got), maxDetailLen+len("…(truncated)"))
	}

	// A multi-byte rune straddling the cut must not leave invalid UTF-8.
	multi := strings.Repeat("é", maxDetailLen)
	if got := truncate(multi); !utf8Valid(got) {
		t.Error("truncate produced invalid UTF-8")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}
