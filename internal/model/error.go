package model

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	CodeUsage        ErrorCode = "usage"
	CodeInvalidInput ErrorCode = "invalid_input"
	CodeUnapproved   ErrorCode = "unapproved_configuration"
	CodeConflict     ErrorCode = "conflict"
	CodeUnavailable  ErrorCode = "unavailable"
	CodeAmbiguous    ErrorCode = "ambiguous_ownership"
	CodeDataLoss     ErrorCode = "unfetched_result"
	CodeInternal     ErrorCode = "internal"
)

type Error struct {
	Code    ErrorCode
	Message string
	Cause   error
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	if err.Message != "" {
		return err.Message
	}
	if err.Cause != nil {
		return err.Cause.Error()
	}
	return string(err.Code)
}

func (err *Error) Unwrap() error { return err.Cause }

func NewError(code ErrorCode, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func ErrorCodeOf(err error) ErrorCode {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return CodeInternal
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch ErrorCodeOf(err) {
	case CodeUsage, CodeInvalidInput:
		return 2
	case CodeUnapproved, CodeConflict, CodeAmbiguous, CodeDataLoss:
		return 3
	case CodeUnavailable:
		return 4
	default:
		return 1
	}
}

func Wrap(code ErrorCode, operation string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Message: fmt.Sprintf("%s: %v", operation, err), Cause: err}
}
