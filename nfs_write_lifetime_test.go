package nfs_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

type writeLifetimeFS struct {
	billy.Filesystem
	openErr, seekErr, writeErr, closeErr error
	mu                                   sync.Mutex
	files                                []*writeLifetimeFile
}

func (fs *writeLifetimeFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag != os.O_RDWR {
		return fs.Filesystem.OpenFile(name, flag, perm)
	}
	if fs.openErr != nil {
		return nil, fs.openErr
	}
	file, err := fs.Filesystem.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	wrapped := &writeLifetimeFile{File: file, owner: fs}
	fs.mu.Lock()
	fs.files = append(fs.files, wrapped)
	fs.mu.Unlock()
	return wrapped, nil
}

type writeLifetimeFile struct {
	billy.File
	owner  *writeLifetimeFS
	closes int32
}

func (f *writeLifetimeFile) Seek(offset int64, whence int) (int64, error) {
	if f.owner.seekErr != nil {
		return 0, f.owner.seekErr
	}
	return f.File.Seek(offset, whence)
}

func (f *writeLifetimeFile) Write(data []byte) (int, error) {
	if f.owner.writeErr != nil {
		// Preserve a real partial write before injecting the backend failure.
		n, err := f.File.Write(data[:1])
		if err != nil {
			return n, err
		}
		return n, f.owner.writeErr
	}
	return f.File.Write(data)
}

func (f *writeLifetimeFile) Close() error {
	atomic.AddInt32(&f.closes, 1)
	if err := f.File.Close(); err != nil {
		return err
	}
	// This fault reports an error after the real descriptor has closed; it
	// does not imply that every billy implementation closes on an error.
	return f.owner.closeErr
}

func TestOnWriteClosesOwnedFile(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		openErr, seekErr, writeErr, closeErr error
		want                                 nfs.NFSStatus
	}{
		{name: "success", want: nfs.NFSStatusOk},
		{name: "open error", openErr: syscall.EACCES, want: nfs.NFSStatusAccess},
		{name: "seek error", seekErr: syscall.EIO, want: nfs.NFSStatusIO},
		{name: "partial write error", writeErr: syscall.EIO, want: nfs.NFSStatusIO},
		{name: "seek and close errors", seekErr: syscall.EIO, closeErr: syscall.ENOSPC, want: nfs.NFSStatusIO},
		{name: "write and close errors", writeErr: syscall.ENOSPC, closeErr: syscall.EIO, want: nfs.NFSStatusNoSPC},
		{name: "close error", closeErr: syscall.EIO, want: nfs.NFSStatusIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "payload"), []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			fs := &writeLifetimeFS{Filesystem: osfs.New(root), openErr: tc.openErr, seekErr: tc.seekErr, writeErr: tc.writeErr, closeErr: tc.closeErr}
			target := serveFS(t, fs)
			// Retain files for the oracle so GC cannot hide a missed Close. On
			// a failing baseline, reclaim only after the assertions below.
			t.Cleanup(func() {
				fs.mu.Lock()
				defer fs.mu.Unlock()
				for _, file := range fs.files {
					if atomic.LoadInt32(&file.closes) == 0 {
						_ = file.Close()
					}
				}
			})
			_, handle, err := target.Lookup("/payload")
			if err != nil {
				t.Fatal(err)
			}
			type writeArgs struct {
				rpc.Header
				Handle []byte
				Offset uint64
				Count  uint32
				How    uint32
				Data   []byte
			}
			data := []byte("updated")
			status := callStatus(t, target, nfs.NFSProcedureWrite, &writeArgs{Header: header(nfs.NFSProcedureWrite), Handle: handle, Offset: 1, Count: uint32(len(data)), How: 0, Data: data})
			if status != tc.want {
				t.Errorf("WRITE status: got %v, want %v", status, tc.want)
			}
			fs.mu.Lock()
			opened := append([]*writeLifetimeFile(nil), fs.files...)
			fs.mu.Unlock()
			wantOpened := 1
			if tc.openErr != nil {
				wantOpened = 0
			}
			if len(opened) != wantOpened {
				t.Fatalf("WRITE opened %d files, want %d", len(opened), wantOpened)
			}
			if tc.openErr == nil && tc.seekErr == nil {
				wantData := "oupdated"
				if tc.writeErr != nil {
					wantData = "ouiginal"
				}
				actual, err := os.ReadFile(filepath.Join(root, "payload"))
				if err != nil || string(actual) != wantData {
					t.Errorf("real file contents: got %q, error %v; want %q", actual, err, wantData)
				}
			}
			for _, file := range opened {
				if got := atomic.LoadInt32(&file.closes); got != 1 {
					t.Errorf("WRITE called Close %d times, want exactly 1", got)
				}
				if _, err := file.File.Seek(0, io.SeekStart); !errors.Is(err, os.ErrClosed) {
					t.Errorf("WRITE left its real file open: Seek error %v, want os.ErrClosed", err)
				}
			}
		})
	}
}
