package nfs

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// writeStability is the level of durability requested with the write
type writeStability uint32

const (
	unstable writeStability = 0
	dataSync writeStability = 1
	fileSync writeStability = 2
)

type writeArgs struct {
	Handle []byte
	Offset uint64
	Count  uint32
	How    uint32
	Data   []byte
}

func onWrite(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = wccDataErrorFormatter
	var req writeArgs
	if err := xdr.Read(w.req.Body, &req); err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	fs, path, err := userHandle.FromHandle(req.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusROFS, os.ErrPermission}
	}
	if len(req.Data) > math.MaxInt32 || req.Count > math.MaxInt32 {
		return &NFSStatusError{NFSStatusFBig, os.ErrInvalid}
	}
	if req.How != uint32(unstable) && req.How != uint32(dataSync) && req.How != uint32(fileSync) {
		return &NFSStatusError{NFSStatusInval, os.ErrInvalid}
	}

	// stat first for pre-op wcc.
	fullPath := fs.Join(path...)
	info, err := fs.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &NFSStatusError{NFSStatusNoEnt, err}
		}
		return &NFSStatusError{NFSStatusAccess, err}
	}
	if !info.Mode().IsRegular() {
		return &NFSStatusError{NFSStatusInval, os.ErrInvalid}
	}
	preOpCache := ToFileAttribute(info, fullPath).AsCache()

	// now the actual op.
	file, err := fs.OpenFile(fs.Join(path...), os.O_RDWR, info.Mode().Perm())
	if err != nil {
		return &NFSStatusError{NFSStatusAccess, err}
	}
	if req.Offset > 0 {
		if _, err := file.Seek(int64(req.Offset), io.SeekStart); err != nil {
			return &NFSStatusError{NFSStatusIO, err}
		}
	}
	end := req.Count
	if len(req.Data) < int(end) {
		end = uint32(len(req.Data))
	}
	writtenCount, err := file.Write(req.Data[:end])
	if err != nil {
		Log.Errorf("Error writing: %v", err)
		return &NFSStatusError{statusFromWriteError(err), err}
	}
	if err := file.Close(); err != nil {
		Log.Errorf("error closing: %v", err)
		return &NFSStatusError{statusFromWriteError(err), err}
	}

	// The stability this reply may claim, which is a promise and not a
	// restatement of the request. RFC 1813, 3.3.7: the reply reports what
	// the server actually achieved, and a client believes it -- Linux puts
	// a page on the commit list only when the reply said UNSTABLE
	// (nfs_write_completion), so a server that answers FILE_SYNC to an
	// unstable write has said "there is nothing left to commit" and will
	// never be sent a COMMIT for it.
	//
	// A filesystem that cannot be told to commit keeps the historical
	// answer: this package's own backends write through, so FILE_SYNC is
	// what they achieve and claiming it costs a client nothing. One that
	// CAN be told holds data a crash could take, so the claim has to be
	// earned: an unstable write is reported as unstable and left for a
	// later COMMIT, and a client that asked for a synchronous write is
	// made to wait for one here.
	committed := fileSync
	if c, ok := fs.(Committer); ok {
		if writeStability(req.How) == unstable {
			committed = unstable
		} else if err := c.Commit(fullPath); err != nil {
			Log.Errorf("Error committing: %v", err)
			return &NFSStatusError{statusFromWriteError(err), err}
		}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := WriteWcc(writer, preOpCache, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := xdr.Write(writer, uint32(writtenCount)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := xdr.Write(writer, committed); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	// The write verifier, which a client keeps beside every unstable write
	// and re-checks at COMMIT: an instance that answers with a different
	// one has restarted, and everything it was holding unstably must be
	// sent again. Server.ID is eight random bytes per Serve, so it changes
	// across restarts, which is the whole of what the verifier is for.
	if err := xdr.Write(writer, w.Server.ID); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
