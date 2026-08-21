package nfs

import (
	"bytes"
	"context"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

func onRemove(ctx context.Context, w *response, userHandle Handler) error {
	return onRemoveObj(ctx, w, userHandle, false)
}

func onRemoveObj(ctx context.Context, w *response, userHandle Handler, directory bool) error {
	w.errorFmt = wccDataErrorFormatter
	obj := DirOpArg{}
	if err := xdr.Read(w.req.Body, &obj); err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	fs, path, err := userHandle.FromHandle(obj.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}

	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusROFS, os.ErrPermission}
	}

	if len(string(obj.Filename)) > PathNameMax {
		return &NFSStatusError{NFSStatusNameTooLong, nil}
	}

	// Lstat, not Stat: a file handle names an object, never whatever a
	// symlink at that name points at. RFC 1813 makes both operands of
	// REMOVE and RMDIR names in a directory — "the file to be removed",
	// not "the file the name resolves to" — and NFSv3 clients resolve
	// symlinks themselves, one LOOKUP at a time, so a server that follows
	// one has resolved a link the client never asked it to.
	fullPath := fs.Join(path...)
	dirInfo, err := fs.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &NFSStatusError{NFSStatusNoEnt, err}
		}
		if os.IsPermission(err) {
			return &NFSStatusError{NFSStatusAccess, err}
		}
		return &NFSStatusError{NFSStatusIO, err}
	}
	if !dirInfo.IsDir() {
		return &NFSStatusError{NFSStatusNotDir, nil}
	}
	preCacheData := ToFileAttribute(dirInfo, fullPath).AsCache()

	// The object being removed, likewise by Lstat. Following here was
	// wrong three ways: a DANGLING symlink answered NFS3ERR_NOENT and was
	// never removed at all (so `rm -rf` of a tree whose link targets sort
	// first left every link behind, and the rmdir after it failed with
	// NFS3ERR_NOTEMPTY, reproducibly, however many times it was retried);
	// a symlink to a directory answered NFS3ERR_ISDIR to REMOVE; and a
	// symlink to a file was removed only because the backend's own Remove
	// does not follow, which is luck rather than agreement.
	toDelete := fs.Join(append(path, string(obj.Filename))...)
	toRemoveStat, err := fs.Lstat(toDelete)
	if err != nil {
		if os.IsNotExist(err) {
			return &NFSStatusError{NFSStatusNoEnt, err}
		}
		if os.IsPermission(err) {
			return &NFSStatusError{NFSStatusAccess, err}
		}
		return &NFSStatusError{NFSStatusIO, err}

	}

	if directory && !toRemoveStat.IsDir() {
		return &NFSStatusError{NFSStatusNotDir, nil}
	} else if !directory && toRemoveStat.IsDir() {
		return &NFSStatusError{NFSStatusIsDir, nil}
	}

	if directory {
		contents, err := fs.ReadDir(toDelete)
		if err != nil {
			if os.IsPermission(err) {
				return &NFSStatusError{NFSStatusAccess, err}
			}
			return &NFSStatusError{NFSStatusIO, err}
		}
		if len(contents) > 0 {
			return &NFSStatusError{NFSStatusNotEmpty, nil}
		}
	}
	toDeleteHandle := userHandle.ToHandle(fs, append(path, string(obj.Filename)))

	err = fs.Remove(toDelete)
	if err != nil {
		if os.IsNotExist(err) {
			return &NFSStatusError{NFSStatusNoEnt, err}
		}
		if os.IsPermission(err) {
			return &NFSStatusError{NFSStatusAccess, err}
		}
		return &NFSStatusError{NFSStatusIO, err}
	}

	// The name is free again, so the verifier of whatever exclusive create
	// last held it must not be honored for the next one.
	exclusiveCreates.forget(verifierKey(fs, toDelete))

	if err := userHandle.InvalidateHandle(fs, toDeleteHandle); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := WriteWcc(writer, preCacheData, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
