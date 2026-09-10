package nfs_test

import (
	"net"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"

	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/helpers"
	"github.com/willscott/go-nfs/helpers/memfs"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// serveFS mounts fs over a loopback server and returns a client for it.
func serveFS(t *testing.T, fs billy.Filesystem) *nfsc.Target {
	t.Helper()

	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = nfs.Serve(listener, helpers.NewCachingHandler(helpers.NewNullAuthHandler(fs), 1024))
	}()
	t.Cleanup(func() { _ = listener.Close() })

	c, err := rpc.DialTCP(listener.Addr().Network(), listener.Addr().(*net.TCPAddr).String(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	var mounter nfsc.Mount
	mounter.Client = c
	target, err := mounter.Mount("/", rpc.AuthNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounter.Unmount() })

	return target
}

// callStatus issues one NFS procedure and returns the status word the server
// put on the wire. The client translates a few statuses into os sentinels and
// collapses the rest, so the tests below read the status directly instead.
func callStatus(t *testing.T, target *nfsc.Target, proc nfs.NFSProcedure, args interface{}) nfs.NFSStatus {
	t.Helper()

	res, err := target.Call(args)
	if err != nil {
		t.Fatalf("%v: %v", proc, err)
	}
	status, err := xdr.ReadUint32(res)
	if err != nil {
		t.Fatalf("reading %v status: %v", proc, err)
	}
	return nfs.NFSStatus(status)
}

func header(proc nfs.NFSProcedure) rpc.Header {
	return rpc.Header{
		Rpcvers: 2,
		Prog:    nfsc.Nfs3Prog,
		Vers:    nfsc.Nfs3Vers,
		Proc:    uint32(proc),
		Cred:    rpc.AuthNull,
		Verf:    rpc.AuthNull,
	}
}

func renameStatus(t *testing.T, target *nfsc.Target, from, to string) nfs.NFSStatus {
	t.Helper()

	fromDir, fromName := path.Split(from)
	toDir, toName := path.Split(to)

	_, fromFH, err := target.Lookup(fromDir)
	if err != nil {
		t.Fatalf("lookup %q: %v", fromDir, err)
	}
	_, toFH, err := target.Lookup(toDir)
	if err != nil {
		t.Fatalf("lookup %q: %v", toDir, err)
	}

	type renameArgs struct {
		rpc.Header
		From nfsc.Diropargs3
		To   nfsc.Diropargs3
	}
	return callStatus(t, target, nfs.NFSProcedureRename, &renameArgs{
		Header: header(nfs.NFSProcedureRename),
		From:   nfsc.Diropargs3{FH: fromFH, Filename: fromName},
		To:     nfsc.Diropargs3{FH: toFH, Filename: toName},
	})
}

func readlinkStatus(t *testing.T, target *nfsc.Target, p string) nfs.NFSStatus {
	t.Helper()

	_, fh, err := target.Lookup(p)
	if err != nil {
		t.Fatalf("lookup %q: %v", p, err)
	}

	type readlinkArgs struct {
		rpc.Header
		Handle []byte
	}
	return callStatus(t, target, nfs.NFSProcedureReadlink, &readlinkArgs{
		Header: header(nfs.NFSProcedureReadlink),
		Handle: fh,
	})
}

// TestOnRenameStatus checks the statuses a rename against a real filesystem
// reaches the client with. os.Rename answers EEXIST for every existing
// directory at the destination, so this covers what a Go-backed filesystem
// can actually report; TestOnRenameStatusFromErrno covers the rest.
func TestOnRenameStatus(t *testing.T) {
	root := t.TempDir()

	mkdir := func(elem ...string) {
		if err := os.MkdirAll(filepath.Join(append([]string{root}, elem...)...), 0755); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(elem ...string) {
		if err := os.WriteFile(filepath.Join(append([]string{root}, elem...)...), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	mkdir("full")
	touch("full", "child")
	mkdir("empty")
	mkdir("outer", "inner")
	touch("file")
	touch("other")

	target := serveFS(t, osfs.New(root))

	for _, tc := range []struct {
		name string
		from string
		to   string
		want nfs.NFSStatus
	}{
		{"directory onto non-empty directory", "/empty", "/full", nfs.NFSStatusExist},
		{"file onto directory", "/file", "/full", nfs.NFSStatusExist},
		{"directory onto file", "/empty", "/file", nfs.NFSStatusNotDir},
		{"directory into itself", "/outer", "/outer/inner/loop", nfs.NFSStatusInval},
		{"missing source", "/absent", "/file", nfs.NFSStatusNoEnt},
		{"file onto file", "/file", "/other", nfs.NFSStatusOk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renameStatus(t, target, tc.from, tc.to); got != tc.want {
				t.Errorf("rename %q -> %q: got %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// renameErrFS reports errs on Rename, standing in for the filesystems that
// call renameat directly and so distinguish the ways a destination can be in
// the way - which os.Rename flattens into EEXIST.
type renameErrFS struct {
	billy.Filesystem
	err error
}

func (r renameErrFS) Rename(oldpath, newpath string) error {
	if r.err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: r.err}
	}
	return r.Filesystem.Rename(oldpath, newpath)
}

// TestOnRenameStatusFromErrno checks that each errno a rename can report
// reaches the client as the status RFC 1813 gives RENAME for it, rather than
// as an NFS3ERR_IO the client can only pass on as EIO.
func TestOnRenameStatusFromErrno(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want nfs.NFSStatus
	}{
		{"destination directory not empty", syscall.ENOTEMPTY, nfs.NFSStatusNotEmpty},
		{"destination exists", syscall.EEXIST, nfs.NFSStatusExist},
		{"destination is a directory", syscall.EISDIR, nfs.NFSStatusIsDir},
		{"destination is not a directory", syscall.ENOTDIR, nfs.NFSStatusNotDir},
		{"across filesystems", syscall.EXDEV, nfs.NFSStatusXDev},
		{"read-only filesystem", syscall.EROFS, nfs.NFSStatusROFS},
		{"out of space", syscall.ENOSPC, nfs.NFSStatusNoSPC},
		{"unwritable", syscall.EACCES, nfs.NFSStatusAccess},
		{"failing disk", syscall.EIO, nfs.NFSStatusIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := memfs.New()
			f, err := mem.Create("/file")
			if err != nil {
				t.Fatal(err)
			}
			_ = f.Close()

			target := serveFS(t, renameErrFS{Filesystem: mem, err: tc.err})
			if got := renameStatus(t, target, "/file", "/renamed"); got != tc.want {
				t.Errorf("rename reporting %v: got %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// readlinkErrFS reports errs on Readlink and, like the OS and unlike memfs,
// does not follow a symlink when stating it. Together those stand in for the
// filesystems whose readlink errno the handler used to throw away in favour
// of what a follow-up Stat implied.
type readlinkErrFS struct {
	billy.Filesystem
	err error
}

func (r readlinkErrFS) Readlink(link string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return r.Filesystem.Readlink(link)
}

func (r readlinkErrFS) Stat(filename string) (os.FileInfo, error) {
	return r.Filesystem.Lstat(filename)
}

// TestOnReadLinkStatusFromErrno checks that the errno readlink reports decides
// the status, even where stating the same name would imply another answer.
func TestOnReadLinkStatusFromErrno(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want nfs.NFSStatus
	}{
		{"not a symlink", syscall.EINVAL, nfs.NFSStatusInval},
		{"parent is not a directory", syscall.ENOTDIR, nfs.NFSStatusNotDir},
		{"vanished", syscall.ENOENT, nfs.NFSStatusNoEnt},
		{"unreadable", syscall.EACCES, nfs.NFSStatusAccess},
		{"failing disk", syscall.EIO, nfs.NFSStatusIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := memfs.New()
			f, err := mem.Create("/target")
			if err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			if err := mem.Symlink("/target", "/link"); err != nil {
				t.Fatal(err)
			}

			target := serveFS(t, readlinkErrFS{Filesystem: mem, err: tc.err})
			if got := readlinkStatus(t, target, "/link"); got != tc.want {
				t.Errorf("readlink of a symlink reporting %v: got %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestOnReadLinkStatusWithoutErrno covers the filesystems that report no errno
// at all: there the handler still has to fall back to what Stat can tell it.
func TestOnReadLinkStatusWithoutErrno(t *testing.T) {
	mem := memfs.New()
	f, err := mem.Create("/regular")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := mem.Symlink("/regular", "/link"); err != nil {
		t.Fatal(err)
	}

	target := serveFS(t, mem)

	// memfs reports "not a symlink" as an opaque error, so only its Stat can
	// say that READLINK was asked of the wrong file type.
	if got := readlinkStatus(t, target, "/regular"); got != nfs.NFSStatusInval {
		t.Errorf("readlink of a regular file: got %v, want %v", got, nfs.NFSStatusInval)
	}
	if got := readlinkStatus(t, target, "/link"); got != nfs.NFSStatusOk {
		t.Errorf("readlink of a symlink: got %v, want %v", got, nfs.NFSStatusOk)
	}
}
