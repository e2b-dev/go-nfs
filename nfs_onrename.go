package nfs

import (
	"bytes"
	"context"
	"os"
	"reflect"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

var doubleWccErrorBody = [16]byte{}

func onRename(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = errFormatterWithBody(doubleWccErrorBody[:])
	from := DirOpArg{}
	err := xdr.Read(w.req.Body, &from)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	fs, fromPath, err := userHandle.FromHandle(ctx, from.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}

	to := DirOpArg{}
	if err = xdr.Read(w.req.Body, &to); err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	fs2, toPath, err := userHandle.FromHandle(ctx, to.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	// Middleware can attach a different request context to each lookup. Only
	// the mounted filesystem's identity determines whether RENAME is allowed;
	// comparing wrapper contents also races mutable filesystem state.
	if !sameFilesystem(fs, fs2) {
		return &NFSStatusError{NFSStatusNotSupp, os.ErrPermission}
	}

	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusROFS, os.ErrPermission}
	}

	if len(string(from.Filename)) > PathNameMax || len(string(to.Filename)) > PathNameMax {
		return &NFSStatusError{NFSStatusNameTooLong, os.ErrInvalid}
	}

	fromDirPath := fs.Join(fromPath...)
	fromDirInfo, err := fs.Stat(fromDirPath)
	if err != nil {
		return statusErrorFrom(err, NFSStatusIO)
	}
	if !fromDirInfo.IsDir() {
		return &NFSStatusError{NFSStatusNotDir, nil}
	}
	preCacheData := ToFileAttribute(fromDirInfo, fromDirPath).AsCache()

	toDirPath := fs.Join(toPath...)
	toDirInfo, err := fs.Stat(toDirPath)
	if err != nil {
		return statusErrorFrom(err, NFSStatusIO)
	}
	if !toDirInfo.IsDir() {
		return &NFSStatusError{NFSStatusNotDir, nil}
	}
	preDestData := ToFileAttribute(toDirInfo, toDirPath).AsCache()

	oldHandle := userHandle.ToHandle(ctx, fs, append(fromPath, string(from.Filename)))

	fromLoc := fs.Join(append(fromPath, string(from.Filename))...)
	toLoc := fs.Join(append(toPath, string(to.Filename))...)

	err = fs.Rename(fromLoc, toLoc)
	if err != nil {
		// rename(2) distinguishes the ways a target can be in the way -
		// EEXIST and ENOTEMPTY for a directory that already holds entries,
		// EISDIR and ENOTDIR for a type mismatch between the two names -
		// and RFC 1813 gives RENAME a status for each. Reporting them as
		// NFS3ERR_IO leaves the client unable to tell a full directory from
		// a failing disk.
		return statusErrorFrom(err, NFSStatusIO)
	}

	if err := userHandle.InvalidateHandle(ctx, fs, oldHandle); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := WriteWcc(writer, preCacheData, tryStat(fs, fromPath)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WriteWcc(writer, preDestData, tryStat(fs, toPath)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}

func sameFilesystem(left, right billy.Filesystem) bool {
	unwrap := func(fs billy.Filesystem) billy.Filesystem {
		for {
			value := reflect.ValueOf(fs)
			if !value.IsValid() || (value.Kind() == reflect.Ptr && value.IsNil()) {
				return nil
			}
			wrapper, ok := fs.(interface{ Unwrap() billy.Filesystem })
			if !ok {
				return fs
			}
			fs = wrapper.Unwrap()
		}
	}
	left, right = unwrap(left), unwrap(right)
	typ := reflect.TypeOf(left)
	// Only nonnil pointer instances have object identity we can prove without
	// reading mutable fields. A struct's type can be comparable while one of
	// its interface fields contains a non-comparable value, so type-level
	// comparability alone does not make interface equality safe.
	return typ != nil && typ == reflect.TypeOf(right) && typ.Kind() == reflect.Ptr && left == right
}
