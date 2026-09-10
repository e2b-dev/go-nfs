package nfs

import (
	"bytes"
	"context"
	"os"

	"github.com/willscott/go-nfs-client/nfs/xdr"
)

func onReadLink(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = opAttrErrorFormatter
	handle, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	fs, path, err := userHandle.FromHandle(handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}

	out, err := fs.Readlink(fs.Join(path...))
	if err != nil {
		// readlink(2) already reports EINVAL for a name that is not a
		// symlink, so the errno describes the failure on its own. Deciding
		// the status from a follow-up Stat instead discards it, and gets the
		// answer wrong on any filesystem whose Stat follows the link.
		if status, ok := statusFromError(err); ok {
			return &NFSStatusError{status, err}
		}
		// Filesystems that are not backed by the OS may report something we
		// cannot name; for those, a Stat that finds a non-symlink still tells
		// us the client asked READLINK of the wrong file type.
		if info, statErr := fs.Stat(fs.Join(path...)); statErr == nil && info.Mode()&os.ModeSymlink == 0 {
			return &NFSStatusError{NFSStatusInval, err}
		}
		return &NFSStatusError{NFSStatusIO, err}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := xdr.Write(writer, out); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
