package notify

import (
	"encoding/json"
	"strconv"
	"time"
)

// marshalCanonical serializes the notification to the exact bytes that are
// both signed and sent. json.Marshal of a struct emits fields in declaration
// order, so the output is deterministic — the signature a receiver recomputes
// over the received body matches byte-for-byte.
func marshalCanonical(n ApprovalNotification) ([]byte, error) {
	return json.Marshal(n)
}

// formatUnix renders t as a unix-seconds string for the signature timestamp.
func formatUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}
