package nfs

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// TestStatusFromError pins the mapping down where it is easy to get wrong:
// syscall.Errno reports both EEXIST and ENOTEMPTY as os.ErrExist and both
// EACCES and EPERM as os.ErrPermission, so a ladder written in the wrong
// order silently answers with the wider errno's status.
func TestStatusFromError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		want   NFSStatus
		wantOk bool
	}{
		{"ENOTEMPTY is not flattened into EEXIST", syscall.ENOTEMPTY, NFSStatusNotEmpty, true},
		{"EEXIST", syscall.EEXIST, NFSStatusExist, true},
		{"EISDIR", syscall.EISDIR, NFSStatusIsDir, true},
		{"ENOTDIR", syscall.ENOTDIR, NFSStatusNotDir, true},
		{"EXDEV", syscall.EXDEV, NFSStatusXDev, true},
		{"ENOENT", syscall.ENOENT, NFSStatusNoEnt, true},
		{"EACCES", syscall.EACCES, NFSStatusAccess, true},
		{"EPERM", syscall.EPERM, NFSStatusAccess, true},
		{"EROFS", syscall.EROFS, NFSStatusROFS, true},
		{"ENAMETOOLONG", syscall.ENAMETOOLONG, NFSStatusNameTooLong, true},
		{"EMLINK", syscall.EMLINK, NFSStatusMlink, true},
		{"ENOSPC", syscall.ENOSPC, NFSStatusNoSPC, true},
		{"EDQUOT", syscall.EDQUOT, NFSStatusDQuot, true},
		{"EINVAL", syscall.EINVAL, NFSStatusInval, true},

		// Filesystems that are not backed by the OS report the os sentinels.
		{"os.ErrExist", os.ErrExist, NFSStatusExist, true},
		{"os.ErrNotExist", os.ErrNotExist, NFSStatusNoEnt, true},
		{"os.ErrPermission", os.ErrPermission, NFSStatusAccess, true},
		{"os.ErrInvalid", os.ErrInvalid, NFSStatusInval, true},

		{"wrapped", fmt.Errorf("rename: %w", syscall.ENOTEMPTY), NFSStatusNotEmpty, true},
		{"in a LinkError", &os.LinkError{Op: "rename", Err: syscall.EISDIR}, NFSStatusIsDir, true},
		{"in a PathError", &os.PathError{Op: "readlink", Err: syscall.EINVAL}, NFSStatusInval, true},

		{"unnameable", errors.New("something went wrong"), NFSStatusIO, false},
		{"EIO", syscall.EIO, NFSStatusIO, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, ok := statusFromError(tc.err)
			if status != tc.want || ok != tc.wantOk {
				t.Errorf("statusFromError(%v) = %v, %v; want %v, %v", tc.err, status, ok, tc.want, tc.wantOk)
			}
		})
	}
}

func TestStatusErrorFrom(t *testing.T) {
	named := statusErrorFrom(syscall.ENOTEMPTY, NFSStatusIO)
	if named.NFSStatus != NFSStatusNotEmpty {
		t.Errorf("named error: got %v, want %v", named.NFSStatus, NFSStatusNotEmpty)
	}

	unnameable := errors.New("something went wrong")
	fallback := statusErrorFrom(unnameable, NFSStatusServerFault)
	if fallback.NFSStatus != NFSStatusServerFault {
		t.Errorf("fallback: got %v, want %v", fallback.NFSStatus, NFSStatusServerFault)
	}

	// The cause has to survive either way, so the server log keeps it even
	// when the status on the wire is the fallback.
	if !errors.Is(fallback, unnameable) {
		t.Error("fallback dropped the wrapped error")
	}
	if !errors.Is(named, syscall.ENOTEMPTY) {
		t.Error("named error dropped the wrapped error")
	}
}
