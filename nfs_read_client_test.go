package nfs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5/osfs"
	nfs "github.com/willscott/go-nfs"
)

func TestRealReadClientTerminalCursor(t *testing.T) {
	for _, prefix := range []int{0, 3} {
		t.Run(fmt.Sprintf("prefix_%d", prefix), func(t *testing.T) {
			root := t.TempDir()
			name := filepath.Join(root, "payload")
			payload := []byte("payload")
			if err := os.WriteFile(name, payload, 0600); err != nil {
				t.Fatal(err)
			}
			target := serveReadBoundary(t, osfs.New(root))
			file, err := target.Open("/payload")
			if err != nil {
				t.Fatal(err)
			}
			if prefix > 0 {
				b := make([]byte, prefix)
				if n, err := file.Read(b); n != prefix || err != nil || !bytes.Equal(b, payload[:prefix]) {
					t.Fatalf("prefix read n=%d err=%v data=%q", n, err, b)
				}
			}
			buf := make([]byte, nfs.CheckRead+1)
			n, err := file.Read(buf)
			if n != len(payload)-prefix || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:n], payload[prefix:]) {
				t.Fatalf("terminal read n=%d err=%v data=%q", n, err, buf[:n])
			}
			if pos, err := file.Seek(0, io.SeekCurrent); pos != int64(len(payload)) || err != nil {
				t.Errorf("actual terminal cursor must advance: pos=%d want=%d err=%v", pos, len(payload), err)
			}
			if n, err := file.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
				t.Errorf("actual next READ must not repeat payload: n=%d err=%v", n, err)
			}
			appended := []byte("appended")
			writer, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := writer.Write(appended)
			closeErr := writer.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("append write=%v close=%v", writeErr, closeErr)
			}
			if n, err := file.Read(buf); n != len(appended) || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:n], appended) {
				t.Errorf("READ after file growth must start at previous end: n=%d err=%v data=%q", n, err, buf[:n])
			}
		})
	}
}

func TestRealReadAtCursorUnaffected(t *testing.T) {
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
	if n, err := file.ReadAt(buf, 0); n != 7 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("ReadAt n=%d err=%v", n, err)
	}
	if pos, err := file.Seek(0, io.SeekCurrent); pos != 0 || err != nil {
		t.Errorf("ReadAt must not move cursor: pos=%d err=%v", pos, err)
	}
	if n, err := file.Read(buf); n != 7 || !errors.Is(err, io.EOF) || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("Read after ReadAt n=%d err=%v", n, err)
	}
}
