package sabnzbd

import "strings"

// contentFailurePrefixes are the fail_message openings SABnzbd writes when the
// release itself is broken, so sending the same NZB again would fail the same
// way (#3024). Each is copied from SABnzbd's own source (sabnzbd/postproc.py,
// sabnzbd/newsunpack.py, sabnzbd/nzb/object.py, sabnzbd/assembler.py) and
// matched as a case insensitive prefix, because SABnzbd appends a file name,
// a block count or a "https://sabnzbd.org/not-complete" link after them.
//
// Deliberately absent, because they are about the client and not the release:
// "Unpacking failed, write error or disk is full?", "Unpacking failed, disk
// full", "Unpacking failed, file too large for filesystem", "Repairing failed,
// Disk full", "Failed to move files", "Post-processing was aborted", script
// exit codes, "Duplicate NZB", and the generic "Unpacking failed, see logfile"
// and "Repairing failed, <exception>" shapes, which can be either.
//
// SABnzbd translates fail_message into its interface language, so a non
// English install matches none of these and keeps the plain failed download
// behaviour. That is the safe direction: an unknown message never blocklists.
//
// SABnzbd separates its own disk faults from archive faults better than NZBGet
// does, but a broken unrar can still look like a CRC error, so the importer
// applies the same per client breaker (contentFailureBreaker) to both.
var contentFailurePrefixes = []string{
	// Missing articles: not on the user's servers, out of retention, or a
	// precheck that found too little available.
	"aborted, cannot be completed",
	"download failed - not on your server(s)",
	"download might fail, only",
	// par2 could not repair it.
	"repair failed, not enough repair blocks",
	"repairing failed, repair failed.",
	// The archive is broken, incomplete or locked.
	"unpacking failed, crc error",
	"unpacking failed, unable to find",
	"unpacking failed, archive requires a password",
	"corrupt rar file",
	"unusable rar file",
	"rar files failed to verify",
	"some files failed to verify against",
	// The job was aborted for what the release contains.
	"aborted, encryption detected",
	"aborted, unwanted extension detected",
}

// IsContentFailure reports whether a SABnzbd history fail_message says the
// release itself is broken rather than the client or the transport.
func IsContentFailure(failMessage string) bool {
	return ContentFailureKind(failMessage) != ""
}

// ContentFailureKind is the matched prefix for a content failure, or "" for
// anything else. Unlike the full message it carries no file name or block
// count, so two releases that failed the same way share one kind.
func ContentFailureKind(failMessage string) string {
	msg := strings.ToLower(strings.TrimSpace(failMessage))
	if msg == "" {
		return ""
	}
	for _, p := range contentFailurePrefixes {
		if strings.HasPrefix(msg, p) {
			return p
		}
	}
	return ""
}
