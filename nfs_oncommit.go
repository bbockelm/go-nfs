package nfs

import (
	"bytes"
	"context"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// Committer is implemented by a billy.Filesystem that does not already
// hold written data somewhere a crash cannot reach -- one that buffers in
// memory, in a mapping, or in a database running with reduced
// synchronicity -- and that can be told to make what it holds durable.
//
// A filesystem that does not implement it is taken at the historical word
// of this package, which is that a Write has reached the backing store's
// own care by the time it returns. That is true of the ones this package
// ships (osfs writes through to a file; memfs has no durability to offer
// either way) and it is what WRITE has always claimed.
//
// # Two procedures consult this, and neither is any use without the other
//
// COMMIT is the obvious one: it exists so that a client which sent
// unstable writes can ask for them to be made durable, and answering it
// without asking the filesystem anything is answering a question about a
// layer this package cannot see.
//
// But a client only sends COMMIT for data the server said was UNSTABLE.
// WRITE replies carry the stability actually achieved (RFC 1813, 3.3.7),
// a client records it (Linux: nfs_write_completion, which puts a page on
// the commit list only for an unstable reply), and a server that answers
// FILE_SYNC to an unstable write has told the client there is nothing
// left to commit. It will never send COMMIT at all, and fsync(2) on the
// mount will return success having caused no server-side durability of
// any kind. So implementing this interface also changes what onWrite
// claims: an unstable write is answered UNSTABLE, and a data-sync or
// file-sync write is committed before the reply.
//
// The cost of that is real, belongs to the filesystem, and is bigger than
// "one commit per fsync". Most of it does not arrive through COMMIT at
// all: a Linux client sends a small file's whole body as a FILE_SYNC write
// rather than an unstable one, to save itself a COMMIT round trip
// (nfs_writepages with FLUSH_COND_STABLE: a single request in the list
// becomes NFS_FILE_SYNC). RFC 1813 makes that a requirement rather than a
// hint, so the server commits inline, once per file, for an application
// that never called fsync. The kernel's own server pays the same toll
// (nfsd_vfs_write sets RWF_SYNC for a stable write). Files large enough to
// need more than one write go out unstable instead and cost one commit at
// close (nfs_file_flush -> nfs_wb_all).
//
// So a filesystem implementing this is opting into a durability pass per
// FILE CREATED on a create-heavy workload, not per fsync. Measured against
// one implementation whose commit is a whole-session sync: 500 small files
// copied onto the mount went from 392ms to 1239ms with its state on a real
// disk, and did not move at all with its state on tmpfs, where fsync is
// free. An implementation that cannot make a repeated commit cheap when
// nothing has changed should not implement the interface.
type Committer interface {
	// Commit makes every write this server has performed on path durable,
	// and returns only once that holds. Committing more than was asked for
	// is explicitly allowed (RFC 1813, 3.3.21: "the server may commit any
	// part of the file it wants"), so an implementation whose durability
	// unit is the whole filesystem may ignore the path.
	Commit(path string) error
}

// onCommit asks the filesystem to make its buffered writes durable and
// reports the write verifier so a client can tell whether the server
// restarted underneath its unstable writes.
//
// The offset and count arguments are read past rather than honored: RFC
// 1813 permits a server to commit more of the file than was requested, and
// the Committer interface is defined in those terms.
func onCommit(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = wccDataErrorFormatter
	handle, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	// The conn will drain the unread offset and count arguments.

	fs, path, err := userHandle.FromHandle(handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusServerFault, os.ErrPermission}
	}

	if c, ok := fs.(Committer); ok {
		if err := c.Commit(fs.Join(path...)); err != nil {
			return &NFSStatusError{statusFromWriteError(err), err}
		}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return err
	}

	// no pre-op cache data.
	if err := xdr.Write(writer, uint32(0)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	// The write verifier. It must differ across server restarts and be
	// stable within one, which is exactly what Server.ID is: eight random
	// bytes minted per Serve when the caller did not set them. A client
	// compares it against the verifier its WRITEs carried and resends
	// anything it sent unstably to a previous instance.
	if err := xdr.Write(writer, w.Server.ID); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
