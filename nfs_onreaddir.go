package nfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"

	"github.com/willscott/go-nfs-client/nfs/xdr"

	"github.com/willscott/go-nfs/file"
)

type readDirArgs struct {
	Handle      []byte
	Cookie      uint64
	CookieVerif uint64
	Count       uint32
}

type readDirEntity struct {
	FileID uint64
	Name   []byte
	Cookie uint64
	Next   bool
}

func onReadDir(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = opAttrErrorFormatter
	obj := readDirArgs{}
	err := xdr.Read(w.req.Body, &obj)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	if obj.Count < 1024 {
		return &NFSStatusError{NFSStatusTooSmall, io.ErrShortBuffer}
	}

	fs, p, err := userHandle.FromHandle(ctx, obj.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}

	contents, verifier, err := getDirListingWithVerifier(ctx, userHandle, obj.Handle, obj.Cookie, obj.CookieVerif)
	if err != nil {
		return err
	}
	if obj.Cookie > 0 && obj.CookieVerif > 0 && verifier != obj.CookieVerif {
		return &NFSStatusError{NFSStatusBadCookie, nil}
	}

	entities := make([]readDirEntity, 0)
	maxBytes := uint32(100) // conservative overhead measure

	started := obj.Cookie == 0
	if started {
		// add '.' and '..' to entities
		dotdotFileID := uint64(0)
		if len(p) > 0 {
			dda := tryStat(fs, p[0:len(p)-1])
			if dda != nil {
				dotdotFileID = dda.Fileid
			}
		}
		dotFileID := uint64(0)
		da := tryStat(fs, p)
		if da != nil {
			dotFileID = da.Fileid
		}
		entities = append(entities,
			readDirEntity{Name: []byte("."), Cookie: 0, Next: true, FileID: dotFileID},
			readDirEntity{Name: []byte(".."), Cookie: 1, Next: true, FileID: dotdotFileID},
		)
	}

	eof := true
	maxEntities := userHandle.HandleLimit() / 2
	for i, c := range contents {
		// cookie equates to index within contents + 2 (for '.' and '..')
		cookie := uint64(i + 2)
		if started {
			maxBytes += 512 // TODO: better estimation.
			if maxBytes > obj.Count || len(entities) > maxEntities {
				eof = false
				break
			}

			attrs := ToFileAttribute(c, path.Join(append(p, c.Name())...))
			entities = append(entities, readDirEntity{
				FileID: attrs.Fileid,
				Name:   []byte(c.Name()),
				Cookie: cookie,
				Next:   true,
			})
		} else if cookie == obj.Cookie {
			started = true
		}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, p)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := xdr.Write(writer, verifier); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := xdr.Write(writer, len(entities) > 0); err != nil { // next
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if len(entities) > 0 {
		entities[len(entities)-1].Next = false
		// no next for last entity

		for _, e := range entities {
			if err := xdr.Write(writer, e); err != nil {
				return &NFSStatusError{NFSStatusServerFault, err}
			}
		}
	}
	if err := xdr.Write(writer, eof); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	// TODO: track writer size at this point to validate maxcount estimation and stop early if needed.

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}

// getDirListingWithVerifier returns the entries of the directory a handle points to.
// When the client continues a listing (cookie > 0), the copy cached on the first page is
// used if there is one. A new listing (cookie 0) always reads the directory.
func getDirListingWithVerifier(ctx context.Context, userHandle Handler, fsHandle []byte, cookie uint64, verifier uint64) ([]fs.FileInfo, uint64, error) {
	// figure out what directory it is.
	fs, p, err := userHandle.FromHandle(ctx, fsHandle)
	if err != nil {
		return nil, 0, &NFSStatusError{NFSStatusStale, err}
	}

	path := fs.Join(p...)
	vh, caching := cachingHandlerOf(userHandle)
	// see if the verifier has this dir cached:
	if caching && cookie != 0 && verifier != 0 {
		entries := vh.DataForVerifier(path, verifier)
		if entries != nil {
			return entries, verifier, nil
		}
	}
	// load the entries.
	contents, err := fs.ReadDir(path)
	if err != nil {
		if os.IsPermission(err) {
			return nil, 0, &NFSStatusError{NFSStatusAccess, err}
		}
		return nil, 0, &NFSStatusError{NFSStatusNotDir, err}
	}

	sort.Slice(contents, func(i, j int) bool {
		return contents[i].Name() < contents[j].Name()
	})

	if caching {
		// let the user handler make a verifier if it can.
		v := vh.VerifierFor(path, contents)
		return contents, v, nil
	}

	id := hashPathAndContents(path, contents)
	return contents, id, nil
}

// cachingHandlerOf returns the CachingHandler inside h, following Unwrap through any
// handlers that wrap it.
func cachingHandlerOf(h Handler) (CachingHandler, bool) {
	for h != nil {
		if c, ok := h.(CachingHandler); ok {
			return c, true
		}
		wrapper, ok := h.(interface{ Unwrap() Handler })
		if !ok {
			break
		}
		h = wrapper.Unwrap()
	}
	return nil, false
}

func hashPathAndContents(path string, contents []fs.FileInfo) uint64 {
	//calculate a cookie-verifier.
	vHash := sha256.New()

	// Add the path to avoid collisions of directories with the same content
	vHash.Write([]byte(path))

	for _, c := range contents {
		vHash.Write([]byte(c.Name())) // Never fails according to the docs
		// Two filesystems can have a directory with the same path and file names.
		// Mixing in each file's ID gives them different verifiers.
		if info := file.GetInfo(c); info != nil {
			vHash.Write(binary.BigEndian.AppendUint64(nil, info.Fileid))
		}
	}

	verify := vHash.Sum(nil)[0:8]
	return binary.BigEndian.Uint64(verify)
}
