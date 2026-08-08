package dag

import (
	"errors"
	"fmt"
)

// DecodeError is one codec-level problem in a definition document,
// qualified by the JSON path of the offending value (e.g.
// "steps[2].config.max_tokens"). An empty Path refers to the document
// itself.
type DecodeError struct {
	Path string
	Msg  string
}

func (e *DecodeError) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

// errList collects DecodeErrors so a decode pass reports every problem it
// can find in one joined error (mirroring internal/config).
type errList struct {
	errs []error
}

// add records a path-qualified error.
func (l *errList) add(path, format string, args ...any) {
	l.errs = append(l.errs, &DecodeError{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// empty reports whether no errors have been recorded.
func (l *errList) empty() bool { return len(l.errs) == 0 }

// join returns all recorded errors as one error, or nil if there are none.
func (l *errList) join() error {
	if l.empty() {
		return nil
	}
	return fmt.Errorf("invalid workflow definition:\n%w", errors.Join(l.errs...))
}
