package protocol

import (
	"errors"
	"fmt"
)

// Failure stage codes for logs and smoke diagnostics.
const (
	CodeCF403        = "cf_403"
	CodeCFBlocked    = "cf_blocked"
	CodeWarm         = "warm"
	CodeGRPCCreate   = "grpc_create"
	CodeGRPCVerify   = "grpc_verify"
	CodeGRPCPassword = "grpc_password"
	CodeSignup       = "signup"
	CodeSignupNoSSO  = "signup_no_sso"
	CodeConfig       = "config"
)

// Fail is a staged error with a stable machine code.
type Fail struct {
	Code string
	Msg  string
	Err  error
}

func (f *Fail) Error() string {
	if f == nil {
		return ""
	}
	if f.Err != nil {
		return fmt.Sprintf("[%s] %s: %v", f.Code, f.Msg, f.Err)
	}
	if f.Msg != "" {
		return fmt.Sprintf("[%s] %s", f.Code, f.Msg)
	}
	return fmt.Sprintf("[%s]", f.Code)
}

func (f *Fail) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Err
}

func Failf(code, msg string, args ...any) error {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	return &Fail{Code: code, Msg: msg}
}

func Wrap(code, msg string, err error) error {
	if err == nil {
		return nil
	}
	var f *Fail
	if errors.As(err, &f) {
		return err
	}
	return &Fail{Code: code, Msg: msg, Err: err}
}
