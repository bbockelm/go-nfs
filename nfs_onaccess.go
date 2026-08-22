package nfs

import (
	"bytes"
	"context"
	"errors"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// The ACCESS3 bits (RFC 1813, 3.3.4). A client asks about a subset of
// them and the reply names the subset it is actually granted.
const (
	// AccessRead: read the file's data, or read a directory's entries.
	AccessRead uint32 = 0x0001
	// AccessLookup: look a name up in a directory. Meaningless on
	// anything that is not a directory.
	AccessLookup uint32 = 0x0002
	// AccessModify: rewrite existing data, or remove and change entries
	// in a directory.
	AccessModify uint32 = 0x0004
	// AccessExtend: append to a file, or add entries to a directory.
	AccessExtend uint32 = 0x0008
	// AccessDelete: remove an entry from a directory. It is a property of
	// the DIRECTORY, never of the object being removed, so it is never
	// granted on a file.
	AccessDelete uint32 = 0x0010
	// AccessExecute: execute a file. Meaningless on a directory, whose
	// search permission is AccessLookup.
	AccessExecute uint32 = 0x0020
)

// Permission is the read/write/execute triple, in the three bits a mode
// nibble uses, which is the shape of the question "what may this server do
// to this object".
type Permission uint32

const (
	// PermissionExecute is execute on a file, search on a directory.
	PermissionExecute Permission = 1
	// PermissionWrite is write.
	PermissionWrite Permission = 2
	// PermissionRead is read.
	PermissionRead Permission = 4
)

// PermissionChecker is implemented by a billy.Filesystem that applies a
// permission model of its own -- mode bits, ownership, ACLs -- and can
// therefore answer ACCESS truthfully. A filesystem that does not implement
// it gets the historical reply, which grants whatever the client asked
// about (minus the write bits when the filesystem is read-only).
//
// This matters because ACCESS is where an NFSv3 client's open(2) is
// decided. There is no OPEN in the protocol: a client answers open(2)
// locally from the ACCESS reply (Linux: nfs_permission -> nfs_do_access),
// so a server that echoes the mask reports "writable" for a file it will
// then refuse to write, and the refusal arrives at the first WRITE
// instead of at open, where every other filesystem puts it.
//
// # What the answer must NOT include
//
// Permitted is asked about the object's own permissions and nothing else.
// In particular an implementation that grants a file's OWNER a bypass on
// the data path -- Linux's in-kernel server does, NFSD_MAY_OWNER_OVERRIDE
// in fs/nfsd/vfs.c, so that a client which has opened a file and then had
// it chmod'd out from under it can still write through the descriptor --
// must NOT apply that bypass here. knfsd is precise about this: the
// operations that read and write data pass NFSD_MAY_OWNER_OVERRIDE
// (nfsd_open), and the ACCESS procedure never does (nfsd_access, which
// calls nfsd_permission with the plain access map). The two halves are one
// design: an honest ACCESS is what makes the client refuse the open, and
// only because the client refuses can the server afford to trust the
// writes that follow one it allowed.
//
// The path names the object itself. Like every other operation whose
// object arrives as a file handle, a terminal symlink is NOT followed: an
// NFSv3 client resolves symlinks itself, one LOOKUP at a time.
type PermissionChecker interface {
	Permitted(path string) (Permission, error)
}

// accessBits maps a granted read/write/execute triple onto the ACCESS3
// bits that follow from it for an object of this type.
//
// The three cases are knfsd's three access maps (fs/nfsd/nfs3proc.c:
// nfs3_regaccess, nfs3_diraccess, nfs3_anyaccess), which is where the
// non-obvious parts come from: changing a directory costs SEARCH as well
// as write, DELETE is a property of the directory an entry lives in and is
// never granted on a file, LOOKUP is never granted on a file, and EXECUTE
// on an object that is neither file nor directory follows READ.
func accessBits(p Permission, t FileType) uint32 {
	r := p&PermissionRead != 0
	w := p&PermissionWrite != 0
	x := p&PermissionExecute != 0

	var granted uint32
	switch t {
	case FileTypeDirectory:
		if r {
			granted |= AccessRead
		}
		if x {
			granted |= AccessLookup
		}
		if w && x {
			granted |= AccessModify | AccessExtend | AccessDelete
		}
	case FileTypeRegular:
		if r {
			granted |= AccessRead
		}
		if w {
			granted |= AccessModify | AccessExtend
		}
		if x {
			granted |= AccessExecute
		}
	default:
		if r {
			granted |= AccessRead | AccessExecute
		}
		if w {
			granted |= AccessModify | AccessExtend
		}
	}
	return granted
}

// grantedAccess asks the filesystem what it permits on path and returns
// the subset of mask that follows.
//
// A refusal is not an error: knfsd's ACCESS loop treats EACCES, EPERM and
// EROFS from its permission check as "this bit is not granted" and keeps
// going, because reporting what is denied is the whole purpose of the
// procedure. Anything else is a real failure of the request.
func grantedAccess(checker PermissionChecker, path string, t FileType, mask uint32) (uint32, error) {
	p, err := checker.Permitted(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrPermission):
		p = 0
	case errors.Is(err, os.ErrNotExist):
		// RFC 1813 does not list NFS3ERR_NOENT for ACCESS: an object
		// named by a handle that no longer resolves is a stale handle.
		return 0, &NFSStatusError{NFSStatusStale, err}
	default:
		return 0, &NFSStatusError{NFSStatusIO, err}
	}
	return mask & accessBits(p, t), nil
}

func onAccess(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = opAttrErrorFormatter
	roothandle, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	fs, path, err := userHandle.FromHandle(roothandle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	mask, err := xdr.ReadUint32(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	attrs := tryStat(fs, path)
	granted := mask
	if checker, ok := fs.(PermissionChecker); ok {
		if attrs == nil {
			// A filesystem that enforces permissions must not be reported
			// as granting them on an object the server cannot even see.
			return &NFSStatusError{NFSStatusStale, os.ErrNotExist}
		}
		granted, err = grantedAccess(checker, fs.Join(path...), attrs.Type, mask)
		if err != nil {
			return err
		}
	}
	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		granted &= AccessRead | AccessLookup | AccessExecute
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, attrs); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := xdr.Write(writer, granted); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
