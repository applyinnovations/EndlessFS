// Package domain contains provider-neutral EndlessFS types and invariants.
package domain

import (
	"errors"
	"fmt"
)

// ErrorKind is the stable error taxonomy shared by providers and use cases.
type ErrorKind string

const (
	ErrorInvalid            ErrorKind = "invalid"
	ErrorUnauthenticated    ErrorKind = "unauthenticated"
	ErrorUnauthorized       ErrorKind = "unauthorized"
	ErrorNotFound           ErrorKind = "not_found"
	ErrorConflict           ErrorKind = "conflict"
	ErrorPreconditionFailed ErrorKind = "precondition_failed"
	ErrorRateLimited        ErrorKind = "rate_limited"
	ErrorUnavailable        ErrorKind = "unavailable"
	ErrorInternal           ErrorKind = "internal"
)

var (
	ErrInvalid            = errors.New("invalid")
	ErrUnauthenticated    = errors.New("unauthenticated")
	ErrUnauthorized       = errors.New("unauthorized")
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("conflict")
	ErrPreconditionFailed = errors.New("precondition failed")
	ErrRateLimited        = errors.New("rate limited")
	ErrUnavailable        = errors.New("unavailable")
	ErrInternal           = errors.New("internal")
)

// Error carries a stable kind and safe diagnostic without provider internals.
type Error struct {
	Kind    ErrorKind
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Kind)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error {
	if e.Cause != nil {
		return e.Cause
	}
	return sentinel(e.Kind)
}

func (e *Error) Is(target error) bool {
	return target == sentinel(e.Kind)
}

// NewError creates a domain error. Message must be safe for normal logs.
func NewError(kind ErrorKind, message string) error {
	return &Error{Kind: kind, Message: message}
}

// WrapError classifies an internal cause without exposing it through Error().
func WrapError(kind ErrorKind, message string, cause error) error {
	return &Error{Kind: kind, Message: message, Cause: cause}
}

// KindOf returns the stable taxonomy value for err.
func KindOf(err error) ErrorKind {
	var domainError *Error
	if errors.As(err, &domainError) {
		return domainError.Kind
	}
	for _, candidate := range []ErrorKind{
		ErrorInvalid,
		ErrorUnauthenticated,
		ErrorUnauthorized,
		ErrorNotFound,
		ErrorConflict,
		ErrorPreconditionFailed,
		ErrorRateLimited,
		ErrorUnavailable,
	} {
		if errors.Is(err, sentinel(candidate)) {
			return candidate
		}
	}
	return ErrorInternal
}

func sentinel(kind ErrorKind) error {
	switch kind {
	case ErrorInvalid:
		return ErrInvalid
	case ErrorUnauthenticated:
		return ErrUnauthenticated
	case ErrorUnauthorized:
		return ErrUnauthorized
	case ErrorNotFound:
		return ErrNotFound
	case ErrorConflict:
		return ErrConflict
	case ErrorPreconditionFailed:
		return ErrPreconditionFailed
	case ErrorRateLimited:
		return ErrRateLimited
	case ErrorUnavailable:
		return ErrUnavailable
	default:
		return ErrInternal
	}
}
