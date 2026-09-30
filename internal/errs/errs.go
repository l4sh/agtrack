// Package errs holds the user-facing error type shared by every layer.
//
// Each error carries an exit code and a stable machine token, so the CLI can
// render it as prose or, under -f json/yaml, as a structured object (D4).
package errs

import (
	"errors"
	"fmt"
)

// Exit codes. Stable: agents branch on them.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitConflict = 4
	ExitDatabase = 5
	ExitSIGINT   = 130
)

// Stable machine tokens for the `error` field of a structured error.
const (
	TokError            = "error"
	TokInvalidArgument  = "invalid_argument"
	TokUsage            = "usage"
	TokNotFound         = "not_found"
	TokAmbiguous        = "ambiguous_reference"
	TokConflict         = "conflict"
	TokDatabase         = "database_error"
	TokDatabaseBusy     = "database_busy"
	TokRequestKeyReused = "request_key_reused"
)

// Error is a user-facing failure. Message is the prose printed after
// "agtrack: "; the other fields feed the structured form.
type Error struct {
	Code    int
	Token   string
	Message string
	Entity  string         // "task", "project", ... when a ref was involved
	Ref     string         // the offending reference as typed
	Extra   map[string]any // candidates, resolved scope, ...
}

func (e *Error) Error() string { return e.Message }

// New is a generic failure (exit 1).
func New(format string, a ...any) *Error {
	return &Error{Code: ExitError, Token: TokInvalidArgument, Message: fmt.Sprintf(format, a...)}
}

// NotFound is a missing entity (exit 3).
func NotFound(entity, ref, format string, a ...any) *Error {
	return &Error{Code: ExitNotFound, Token: TokNotFound, Entity: entity, Ref: ref,
		Message: fmt.Sprintf(format, a...)}
}

// Conflict is a collision with existing data (exit 4).
func Conflict(format string, a ...any) *Error {
	return &Error{Code: ExitConflict, Token: TokConflict, Message: fmt.Sprintf(format, a...)}
}

// Usage is a command-line syntax error (exit 2).
func Usage(format string, a ...any) *Error {
	return &Error{Code: ExitUsage, Token: TokUsage, Message: fmt.Sprintf(format, a...)}
}

// With sets the entity/ref on an error and returns it, for chaining.
func (e *Error) With(entity, ref string) *Error {
	e.Entity, e.Ref = entity, ref
	return e
}

// WithToken overrides the machine token.
func (e *Error) WithToken(tok string) *Error {
	e.Token = tok
	return e
}

// As extracts an *Error from err, if it is one.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsNotFound reports whether err is a not-found *Error.
func IsNotFound(err error) bool {
	e, ok := As(err)
	return ok && e.Code == ExitNotFound
}
