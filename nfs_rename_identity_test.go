package nfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

type renameRequestFS struct {
	billy.Filesystem
	ctx context.Context
}

func (fs *renameRequestFS) Unwrap() billy.Filesystem { return fs.Filesystem }

type renameIdentityHandler struct {
	Handler
	from, to    billy.Filesystem
	invalidated bool
}

func (h *renameIdentityHandler) FromHandle(_ context.Context, handle []byte) (billy.Filesystem, []string, error) {
	if bytes.Equal(handle, []byte{1}) {
		return h.from, []string{"from"}, nil
	}
	return h.to, []string{"to"}, nil
}

func (h *renameIdentityHandler) ToHandle(context.Context, billy.Filesystem, []string) []byte {
	return []byte{3}
}

func (h *renameIdentityHandler) InvalidateHandle(context.Context, billy.Filesystem, []byte) error {
	h.invalidated = true
	return nil
}

func TestRenameUsesMountedFilesystemIdentity(t *testing.T) {
	for _, differentMount := range []bool{false, true} {
		name := "request-wrappers"
		if differentMount {
			name = "different-mounts-with-same-root"
		}
		t.Run(name, func(t *testing.T) {
			left := memfs.New()
			right := left
			if differentMount {
				right = memfs.New()
			}
			if left.Root() != right.Root() {
				t.Fatal("test requires mounts with the same root string")
			}
			if err := left.MkdirAll("from", 0755); err != nil {
				t.Fatal(err)
			}
			if err := right.MkdirAll("to", 0755); err != nil {
				t.Fatal(err)
			}
			file, err := left.Create("from/source")
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.Write([]byte("preserved data"))
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("write source: %v; close: %v", writeErr, closeErr)
			}
			oldCtx, cancel := context.WithCancel(context.Background())
			cancel()
			handler := &renameIdentityHandler{
				from: &renameRequestFS{Filesystem: left, ctx: oldCtx},
				to:   &renameRequestFS{Filesystem: &renameRequestFS{Filesystem: right, ctx: context.Background()}, ctx: context.Background()},
			}
			body := new(bytes.Buffer)
			for _, arg := range []DirOpArg{{Handle: []byte{1}, Filename: []byte("source")}, {Handle: []byte{2}, Filename: []byte("destination")}} {
				if err := xdr.Write(body, arg); err != nil {
					t.Fatal(err)
				}
			}
			writer := new(bytes.Buffer)
			err = onRename(context.Background(), &response{req: &request{Body: body}, writer: writer, responded: true}, handler)
			if differentMount {
				var status *NFSStatusError
				if !errors.As(err, &status) || status.NFSStatus != NFSStatusNotSupp || !errors.Is(err, os.ErrPermission) {
					t.Fatalf("cross-mount RENAME = %v, want NFS3ERR_NOTSUPP", err)
				}
				if handler.invalidated {
					t.Fatal("rejected rename invalidated a handle")
				}
				if _, err := left.Stat("from/source"); err != nil {
					t.Fatal("rejected rename changed the source:", err)
				}
				if _, err := right.Stat("to/destination"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected rename created a destination: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal("same-mount rename through different request wrappers:", err)
			}
			var status uint32
			if err := xdr.Read(writer, &status); err != nil || status != uint32(NFSStatusOk) {
				t.Fatalf("rename response = %d, %v", status, err)
			}
			if !handler.invalidated {
				t.Fatal("successful rename did not invalidate the old handle")
			}
			if _, err := left.Stat("from/source"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source still exists after rename: %v", err)
			}
			file, err = right.Open("to/destination")
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(file)
			closeErr = file.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read destination: %v; close: %v", readErr, closeErr)
			}
			if string(data) != "preserved data" {
				t.Fatalf("renamed data = %q", data)
			}
		})
	}
}

// A slice-valued filesystem cannot be compared for object identity, even when
// two values embed the same backend. Do not inspect its mutable contents.
type nonComparableRenameFS struct {
	billy.Filesystem
	markers []int
}

// The struct type is comparable, but its interface can contain a value that
// is not. Type.Comparable alone does not make interface equality panic-safe.
type comparableRenameFS struct {
	billy.Filesystem
}

func TestFilesystemIdentityRejectsUnprovableIdentity(t *testing.T) {
	fs := nonComparableRenameFS{Filesystem: memfs.New(), markers: []int{1}}
	if sameFilesystem(fs, fs) {
		t.Fatal("non-comparable filesystem was accepted by content equality")
	}
	if sameFilesystem(nil, nil) {
		t.Fatal("nil filesystems have no mounted identity")
	}
	valueWithInterface := comparableRenameFS{Filesystem: fs}
	if sameFilesystem(valueWithInterface, valueWithInterface) {
		t.Fatal("comparable value with a non-comparable interface has no provable object identity")
	}
	comparableValue := comparableRenameFS{Filesystem: memfs.New()}
	if sameFilesystem(comparableValue, comparableValue) {
		t.Fatal("value equality does not establish mounted object identity")
	}
	var nilWrapper *renameRequestFS
	if sameFilesystem(nilWrapper, nilWrapper) {
		t.Fatal("typed nil wrapper has no mounted identity")
	}
	if sameFilesystem(&renameRequestFS{Filesystem: nilWrapper}, nilWrapper) {
		t.Fatal("nested typed nil wrapper has no mounted identity")
	}
	// Pointer identity remains safe even if the pointed-to object's fields
	// include an interface with a slice-valued dynamic filesystem.
	object := &comparableRenameFS{Filesystem: fs}
	if !sameFilesystem(object, object) {
		t.Fatal("the exact same nonnil mounted object must retain its identity")
	}
}
