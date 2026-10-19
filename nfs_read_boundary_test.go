package nfs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	nfs "github.com/willscott/go-nfs"
	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
	"github.com/willscott/go-nfs/helpers"
)

type readBoundaryFS struct {
	billy.Filesystem
	mu                              sync.Mutex
	files                           []*readBoundaryFile
	statError, attrError, readError int32
}

type readBoundaryFile struct {
	billy.File
	owner     *readBoundaryFS
	lengths   []int
	closed    chan struct{}
	closeOnce sync.Once
	closes    int32
}

func (fs *readBoundaryFS) Open(name string) (billy.File, error) {
	file, err := fs.Filesystem.Open(name)
	if err != nil {
		return nil, err
	}
	wrapped := &readBoundaryFile{File: file, owner: fs, closed: make(chan struct{})}
	fs.mu.Lock()
	fs.files = append(fs.files, wrapped)
	fs.mu.Unlock()
	return wrapped, nil
}

func (fs *readBoundaryFS) Stat(name string) (os.FileInfo, error) {
	if atomic.LoadInt32(&fs.statError) != 0 {
		return nil, syscall.EACCES
	}
	return fs.Filesystem.Stat(name)
}

func (fs *readBoundaryFS) Lstat(name string) (os.FileInfo, error) {
	if atomic.LoadInt32(&fs.attrError) != 0 {
		return nil, syscall.EIO
	}
	return fs.Filesystem.Lstat(name)
}

func (f *readBoundaryFile) ReadAt(p []byte, off int64) (int, error) {
	f.owner.mu.Lock()
	f.lengths = append(f.lengths, len(p))
	f.owner.mu.Unlock()
	if atomic.LoadInt32(&f.owner.readError) != 0 {
		return 0, syscall.EIO
	}
	return f.File.ReadAt(p, off)
}

func (f *readBoundaryFile) Close() error {
	atomic.AddInt32(&f.closes, 1)
	err := f.File.Close()
	f.closeOnce.Do(func() { close(f.closed) })
	return err
}

type readBoundaryConn struct {
	net.Conn
	disconnected chan struct{}
}

type readBoundaryListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*readBoundaryConn
}

func (l *readBoundaryListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &readBoundaryConn{Conn: conn, disconnected: make(chan struct{})}
	l.mu.Lock()
	l.conns = append(l.conns, wrapped)
	l.mu.Unlock()
	return wrapped, nil
}

func awaitReadBoundary(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Errorf("%s did not settle", what)
	}
}

func serveReadBoundary(t *testing.T, fs billy.Filesystem) *nfsc.Target {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tracked := &readBoundaryListener{Listener: listener}
	ctx, cancel := context.WithCancel(context.Background())
	server := &nfs.Server{
		Handler: helpers.NewCachingHandler(helpers.NewNullAuthHandler(fs), 16),
		Context: ctx,
		OnDisconnect: func(_ context.Context, conn net.Conn) {
			close(conn.(*readBoundaryConn).disconnected)
		},
	}
	acceptDone := make(chan struct{})
	go func() { defer close(acceptDone); _ = server.Serve(tracked) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		awaitReadBoundary(t, acceptDone, "server accept loop")
		tracked.mu.Lock()
		connections := append([]*readBoundaryConn(nil), tracked.conns...)
		tracked.mu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
			awaitReadBoundary(t, conn.disconnected, "server request loop")
		}
	})
	var client *rpc.Client
	for attempt := 0; attempt < 8; attempt++ {
		client, err = rpc.DialTCP("tcp", listener.Addr().String(), false)
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	mount := nfsc.Mount{Client: client}
	target, err := mount.Mount("/", rpc.AuthNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mount.Unmount() })
	return target
}

func TestReadFileBoundary(t *testing.T) {
	payload := []byte("payload")
	for _, tc := range []struct {
		name                            string
		data                            []byte
		offset                          uint64
		count                           uint32
		want                            []byte
		wantEOF                         uint32
		wantStatus                      nfs.NFSStatus
		statError, attrError, readError bool
	}{
		{name: "large tail", data: payload, offset: 1, count: nfs.CheckRead + 1, want: payload[1:], wantEOF: 1},
		{name: "at EOF", data: payload, offset: uint64(len(payload)), count: nfs.CheckRead + 1, wantEOF: 1},
		{name: "beyond EOF", data: payload, offset: uint64(len(payload) + 1), count: nfs.CheckRead + 1, wantEOF: 1},
		{name: "empty", count: nfs.CheckRead + 1, wantEOF: 1},
		{name: "post attributes unavailable", data: payload, count: nfs.CheckRead + 1, want: payload, wantEOF: 1, attrError: true},
		{name: "stat error", data: payload, count: nfs.CheckRead + 1, wantStatus: nfs.NFSStatusAccess, statError: true},
		{name: "read error", data: payload, count: 2, wantStatus: nfs.NFSStatusIO, readError: true},
		{name: "small short read", data: payload, offset: 1, count: 10, want: payload[1:], wantEOF: 1},
		{name: "within file", data: payload, offset: 1, count: 2, want: payload[1:3]},
		{name: "exact file size", data: payload, count: uint32(len(payload)), want: payload, wantEOF: 1},
		{name: "zero before EOF", data: payload},
		{name: "zero at EOF", data: payload, offset: uint64(len(payload)), wantEOF: 1},
		{name: "offset outside signed range", data: payload, offset: uint64(1 << 63), count: 1, wantEOF: 1},
		{name: "maximum offset", data: payload, offset: ^uint64(0), count: nfs.CheckRead + 1, wantEOF: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "payload"), tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			fs := &readBoundaryFS{Filesystem: osfs.New(root)}
			target := serveReadBoundary(t, fs)
			_, handle, err := target.Lookup("/payload")
			if err != nil {
				t.Fatal(err)
			}
			if tc.statError {
				atomic.StoreInt32(&fs.statError, 1)
			}
			if tc.attrError {
				atomic.StoreInt32(&fs.attrError, 1)
			}
			if tc.readError {
				atomic.StoreInt32(&fs.readError, 1)
			}
			res, err := target.Call(&struct {
				rpc.Header
				Handle []byte
				Offset uint64
				Count  uint32
			}{header(nfs.NFSProcedureRead), handle, tc.offset, tc.count})
			if err != nil {
				t.Fatal(err)
			}
			var status uint32
			var result struct {
				Attr  nfsc.PostOpAttr
				Count uint32
				EOF   uint32
			}
			if err := xdr.Read(res, &status); err != nil {
				t.Fatal(err)
			}
			if status != uint32(tc.wantStatus) {
				t.Errorf("READ status must match: got %d want %d", status, tc.wantStatus)
			}
			if status == uint32(nfs.NFSStatusOk) {
				if err := xdr.Read(res, &result); err != nil {
					t.Fatal(err)
				}
				dataLength, err := xdr.ReadUint32(res)
				if err != nil || dataLength > tc.count {
					t.Fatalf("READ opaque length=%d request=%d err=%v", dataLength, tc.count, err)
				}
				data := make([]byte, dataLength)
				if dataLength != 0 {
					if _, err := io.ReadFull(res, data); err != nil {
						t.Fatal(err)
					}
				}
				if result.Count != uint32(len(data)) || !bytes.Equal(data, tc.want) {
					t.Errorf("READ Count/Data disagreement: count=%d data=%q want=%q", result.Count, data, tc.want)
				}
				if result.EOF != tc.wantEOF {
					t.Errorf("READ must truthfully report EOF: got %d want %d", result.EOF, tc.wantEOF)
				}
			}
			fs.mu.Lock()
			files := append([]*readBoundaryFile(nil), fs.files...)
			fs.mu.Unlock()
			if len(files) != 1 {
				t.Fatalf("READ must own one real file, got %d", len(files))
			}
			awaitReadBoundary(t, files[0].closed, "READ file Close")
			if atomic.LoadInt32(&files[0].closes) != 1 {
				t.Errorf("READ must close its real file exactly once")
			}
			fs.mu.Lock()
			for _, length := range files[0].lengths {
				if uint64(length) > uint64(tc.count) || (tc.offset >= uint64(len(tc.data)) && tc.count > nfs.CheckRead && length != 0) {
					t.Errorf("READ allocation must not grow past the request at EOF: backend length=%d request=%d", length, tc.count)
				}
			}
			fs.mu.Unlock()
		})
	}
}

func TestReadClientReachesEOF(t *testing.T) {
	root := t.TempDir()
	payload := []byte("payload")
	if err := os.WriteFile(filepath.Join(root, "payload"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	target := serveReadBoundary(t, osfs.New(root))
	file, err := target.Open("/payload")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, nfs.CheckRead+1)
	var data []byte
	reachedEOF := false
	zeroProgress := 0
	reads := 0
	// Fixed three-call bound: never start an unbounded copy/read loop.
	for attempt := 0; attempt < 3; attempt++ {
		n, err := file.Read(buf)
		reads++
		data = append(data, buf[:n]...)
		if errors.Is(err, io.EOF) {
			reachedEOF = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			zeroProgress++
		}
	}
	t.Logf("bounded real client: reads=%d payload=%d EOF=%v zero-progress=%d", reads, len(data), reachedEOF, zeroProgress)
	if reads != 1 {
		t.Errorf("real client must receive terminal EOF with the payload READ: got %d calls", reads)
	}
	if !bytes.Equal(data, payload) || !reachedEOF || zeroProgress != 0 {
		t.Errorf("real client must reach EOF without zero progress: data=%q EOF=%v zero-progress=%d", data, reachedEOF, zeroProgress)
	}
}

func TestReadAdvertisedLimit(t *testing.T) {
	target := serveReadBoundary(t, osfs.New(t.TempDir()))
	info, err := target.FSInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.RTMax != nfs.MaxRead || info.RTPref > nfs.MaxRead {
		t.Errorf("FSINFO must advertise the actual READ limit: max=%d pref=%d enforced=%d", info.RTMax, info.RTPref, nfs.MaxRead)
	}
}
