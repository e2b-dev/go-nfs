package nfs

import (
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// RPCError provides the error interface for errors thrown by
// procedures to be transmitted over the XDR RPC channel
type RPCError interface {
	// An RPCError is an `error` with this method
	Error() string
	// Code is the RPC Response code to send
	Code() ResponseCode
	// BinaryMarshaler is the on-wire representation of this error
	encoding.BinaryMarshaler
}

// AuthStat is an enumeration of why authentication ahs failed
type AuthStat uint32

// AuthStat Codes
const (
	AuthStatOK AuthStat = iota
	AuthStatBadCred
	AuthStatRejectedCred
	AuthStatBadVerifier
	AuthStatRejectedVerfier
	AuthStatTooWeak
	AuthStatInvalidResponse
	AuthStatFailed
	AuthStatKerbGeneric
	AuthStatTimeExpire
	AuthStatTktFile
	AuthStatDecode
	AuthStatNetAddr
	AuthStatRPCGSSCredProblem
	AuthStatRPCGSSCTXProblem
)

// AuthError is an RPCError
type AuthError struct {
	AuthStat
}

// Code for AuthErrors is ResponseCodeAuthError
func (a *AuthError) Code() ResponseCode {
	return ResponseCodeAuthError
}

// Error is a textual representaiton of the auth error. From the RFC
func (a *AuthError) Error() string {
	switch a.AuthStat {
	case AuthStatOK:
		return "Auth Status: OK"
	case AuthStatBadCred:
		return "Auth Status: bad credential"
	case AuthStatRejectedCred:
		return "Auth Status: client must begin new session"
	case AuthStatBadVerifier:
		return "Auth Status: bad verifier"
	case AuthStatRejectedVerfier:
		return "Auth Status: verifier expired or replayed"
	case AuthStatTooWeak:
		return "Auth Status: rejected for security reasons"
	case AuthStatInvalidResponse:
		return "Auth Status: bogus response verifier"
	case AuthStatFailed:
		return "Auth Status: reason unknown"
	case AuthStatKerbGeneric:
		return "Auth Status: kerberos generic error"
	case AuthStatTimeExpire:
		return "Auth Status: time of credential expired"
	case AuthStatTktFile:
		return "Auth Status: problem with ticket file"
	case AuthStatDecode:
		return "Auth Status: can't decode authenticator"
	case AuthStatNetAddr:
		return "Auth Status: wrong net address in ticket"
	case AuthStatRPCGSSCredProblem:
		return "Auth Status: no credentials for user"
	case AuthStatRPCGSSCTXProblem:
		return "Auth Status: problem with context"
	}
	return "Auth Status: Unknown"
}

// MarshalBinary sends the specific auth status
func (a *AuthError) MarshalBinary() (data []byte, err error) {
	var resp [4]byte
	binary.LittleEndian.PutUint32(resp[:], uint32(a.AuthStat))
	return resp[:], nil
}

// RPCMismatchError is an RPCError
type RPCMismatchError struct {
	Low  uint32
	High uint32
}

// Code for RPCMismatchError is ResponseCodeRPCMismatch
func (r *RPCMismatchError) Code() ResponseCode {
	return ResponseCodeRPCMismatch
}

func (r *RPCMismatchError) Error() string {
	return fmt.Sprintf("RPC Mismatch: Expected version between %d and %d.", r.Low, r.High)
}

// MarshalBinary sends the specific rpc mismatch range
func (r *RPCMismatchError) MarshalBinary() (data []byte, err error) {
	var resp [8]byte
	binary.LittleEndian.PutUint32(resp[0:4], uint32(r.Low))
	binary.LittleEndian.PutUint32(resp[4:8], uint32(r.High))
	return resp[:], nil
}

// ResponseCodeProcUnavailableError is an RPCError
type ResponseCodeProcUnavailableError struct {
}

// Code for ResponseCodeProcUnavailableError
func (r *ResponseCodeProcUnavailableError) Code() ResponseCode {
	return ResponseCodeProcUnavailable
}

func (r *ResponseCodeProcUnavailableError) Error() string {
	return "The requested procedure is unexported"
}

// MarshalBinary - this error has no associated body
func (r *ResponseCodeProcUnavailableError) MarshalBinary() (data []byte, err error) {
	return []byte{}, nil
}

// ResponseCodeSystemError is an RPCError
type ResponseCodeSystemError struct {
}

// Code for ResponseCodeSystemError
func (r *ResponseCodeSystemError) Code() ResponseCode {
	return ResponseCodeSystemErr
}

func (r *ResponseCodeSystemError) Error() string {
	return "memory allocation failure"
}

// MarshalBinary - this error has no associated body
func (r *ResponseCodeSystemError) MarshalBinary() (data []byte, err error) {
	return []byte{}, nil
}

// basicErrorFormatter is the default error handler for response errors.
// if the error is already formatted, it is directly written. Otherwise,
// ResponseCodeSystemError is sent to the client.
func basicErrorFormatter(err error) RPCError {
	var rpcErr RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	return &ResponseCodeSystemError{}
}

// NFSStatusError represents an error at the NFS level.
type NFSStatusError struct {
	NFSStatus
	WrappedErr error
}

// Error is The wrapped error
func (s *NFSStatusError) Error() string {
	message := s.NFSStatus.String()
	if s.WrappedErr != nil {
		message = fmt.Sprintf("%s: %v", message, s.WrappedErr)
	}
	return message
}

// Code for NFS issues are successful RPC responses
func (s *NFSStatusError) Code() ResponseCode {
	return ResponseCodeSuccess
}

// MarshalBinary - The binary form of the code.
func (s *NFSStatusError) MarshalBinary() (data []byte, err error) {
	var resp [4]byte
	binary.BigEndian.PutUint32(resp[0:4], uint32(s.NFSStatus))
	return resp[:], nil
}

// Unwrap unpacks wrapped errors
func (s *NFSStatusError) Unwrap() error {
	return s.WrappedErr
}

// StatusErrorWithBody is an NFS error with a payload.
type StatusErrorWithBody struct {
	NFSStatusError
	Body []byte
}

// MarshalBinary provides the wire format of the error response
func (s *StatusErrorWithBody) MarshalBinary() (data []byte, err error) {
	head, err := s.NFSStatusError.MarshalBinary()
	return append(head, s.Body...), err
}

// errFormatterWithBody appends a provided body to errors
func errFormatterWithBody(body []byte) func(err error) RPCError {
	return func(err error) RPCError {
		if nerr, ok := err.(*NFSStatusError); ok {
			return &StatusErrorWithBody{*nerr, body[:]}
		}
		var rErr RPCError
		if errors.As(err, &rErr) {
			return rErr
		}
		return &ResponseCodeSystemError{}
	}
}

var (
	opAttrErrorBody       = [4]byte{}
	opAttrErrorFormatter  = errFormatterWithBody(opAttrErrorBody[:])
	wccDataErrorBody      = [8]byte{}
	wccDataErrorFormatter = errFormatterWithBody(wccDataErrorBody[:])
)

// statusFromWriteError maps write errors to NFS status codes
func statusFromWriteError(err error) NFSStatus {
	if err == nil {
		return NFSStatusOk
	}
	if errors.Is(err, syscall.ENOSPC) {
		return NFSStatusNoSPC
	}
	if errors.Is(err, syscall.EDQUOT) {
		return NFSStatusDQuot
	}
	if errors.Is(err, syscall.EFBIG) {
		return NFSStatusFBig
	}
	return NFSStatusIO
}

// statusFromError maps a filesystem error to the NFS status that names the
// condition the client hit. err must not be nil. The second return is false
// when the error carries no status a client can act on, leaving the choice of
// fallback to the operation, whose RFC 1813 error list says what it may send.
//
// Order matters wherever a sentinel spans more than one errno: syscall.Errno
// reports both EEXIST and ENOTEMPTY as os.ErrExist and both EACCES and EPERM
// as os.ErrPermission, so the narrower errno is tested first. The sentinels
// are tested alongside the errnos because billy filesystems that are not
// backed by the OS return those instead.
//
// Operations that can only fail in a handful of ways keep their own narrower
// mapping (see statusFromWriteError) so they cannot answer with a status the
// client does not expect from them.
func statusFromError(err error) (NFSStatus, bool) {
	switch {
	case errors.Is(err, syscall.ENOTEMPTY):
		return NFSStatusNotEmpty, true
	case errors.Is(err, syscall.EEXIST), errors.Is(err, os.ErrExist):
		return NFSStatusExist, true
	case errors.Is(err, syscall.EISDIR):
		return NFSStatusIsDir, true
	case errors.Is(err, syscall.ENOTDIR):
		return NFSStatusNotDir, true
	case errors.Is(err, syscall.EXDEV):
		return NFSStatusXDev, true
	case errors.Is(err, syscall.ENOENT), errors.Is(err, os.ErrNotExist):
		return NFSStatusNoEnt, true
	case errors.Is(err, os.ErrPermission):
		return NFSStatusAccess, true
	case errors.Is(err, syscall.EROFS):
		return NFSStatusROFS, true
	case errors.Is(err, syscall.ENAMETOOLONG):
		return NFSStatusNameTooLong, true
	case errors.Is(err, syscall.EMLINK):
		return NFSStatusMlink, true
	case errors.Is(err, syscall.ENOSPC):
		return NFSStatusNoSPC, true
	case errors.Is(err, syscall.EDQUOT):
		return NFSStatusDQuot, true
	case errors.Is(err, syscall.EINVAL), errors.Is(err, os.ErrInvalid):
		return NFSStatusInval, true
	}
	return NFSStatusIO, false
}

// statusErrorFrom pairs a filesystem error with the NFS status that names it,
// falling back to the status the operation prefers for errors statusFromError
// cannot name. The original error is always wrapped, so it stays available to
// the server log even when the status on the wire is the fallback.
func statusErrorFrom(err error, fallback NFSStatus) *NFSStatusError {
	if status, ok := statusFromError(err); ok {
		return &NFSStatusError{status, err}
	}
	return &NFSStatusError{fallback, err}
}
