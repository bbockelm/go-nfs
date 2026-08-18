package nfs

import (
	"bytes"
	"context"
	"os"
	"reflect"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// linkErrorBody is LINK3resfail: a post_op_attr for the file followed by
// wcc_data for the link directory, both "not present". It is four bytes
// longer than the bare wcc_data body the other directory operations use,
// and a client handed the short form cannot decode the reply at all --
// it reports EIO instead of the status the server chose.
var linkErrorBody = [12]byte{}

// HardLinker is implemented by a billy.Filesystem that can create hard
// links. LINK names its source by file handle, which this package resolves
// to a path, so the method takes two paths -- the same shape as os.Link.
//
// UnixChange carries the same operation for backends that expose it
// through Handler.Change; either one is enough.
type HardLinker interface {
	Link(oldname, newname string) error
}

// hardLinker returns the hard-link implementation to use for fs, or nil if
// the backend cannot make hard links.
func hardLinker(fs billy.Filesystem, changer billy.Change) func(oldname, newname string) error {
	if l, ok := fs.(HardLinker); ok {
		return l.Link
	}
	if c, ok := changer.(UnixChange); ok {
		return c.Link
	}
	return nil
}

func onLink(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = errFormatterWithBody(linkErrorBody[:])

	// LINK3args (RFC 1813, 3.3.15) is `nfs_fh3 file` followed by
	// `diropargs3 link`. There is no sattr3 in this message.
	fileHandle, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	link := DirOpArg{}
	if err := xdr.Read(w.req.Body, &link); err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	fs, source, err := userHandle.FromHandle(fileHandle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	linkFS, dirPath, err := userHandle.FromHandle(link.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	// A hard link cannot span filesystems.
	if !reflect.DeepEqual(fs, linkFS) {
		return &NFSStatusError{NFSStatusXDev, os.ErrInvalid}
	}
	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusROFS, os.ErrPermission}
	}
	if len(string(link.Filename)) > PathNameMax {
		return &NFSStatusError{NFSStatusNameTooLong, os.ErrInvalid}
	}

	linker := hardLinker(fs, userHandle.Change(fs))
	if linker == nil {
		return &NFSStatusError{NFSStatusNotSupp, os.ErrPermission}
	}

	sourcePath := fs.Join(source...)
	sourceInfo, err := fs.Lstat(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &NFSStatusError{NFSStatusNoEnt, err}
		}
		if os.IsPermission(err) {
			return &NFSStatusError{NFSStatusAccess, err}
		}
		return &NFSStatusError{NFSStatusIO, err}
	}
	if sourceInfo.IsDir() {
		return &NFSStatusError{NFSStatusIsDir, nil}
	}

	dirFullPath := fs.Join(dirPath...)
	dirInfo, err := fs.Stat(dirFullPath)
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
	preCacheData := ToFileAttribute(dirInfo, dirFullPath).AsCache()

	newFile := append(dirPath, string(link.Filename))
	newFilePath := fs.Join(newFile...)
	if _, err := fs.Lstat(newFilePath); err == nil {
		return &NFSStatusError{NFSStatusExist, os.ErrExist}
	}

	if err := linker(sourcePath, newFilePath); err != nil {
		if os.IsNotExist(err) {
			return &NFSStatusError{NFSStatusNoEnt, err}
		}
		if os.IsExist(err) {
			return &NFSStatusError{NFSStatusExist, err}
		}
		if os.IsPermission(err) {
			return &NFSStatusError{NFSStatusAccess, err}
		}
		return &NFSStatusError{NFSStatusIO, err}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	// LINK3resok is the file's post_op_attr and the directory's wcc_data.
	// It carries no file handle: the client already holds one for the
	// object it asked to link.
	if err := WritePostOpAttrs(writer, tryStat(fs, source)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WriteWcc(writer, preCacheData, tryStat(fs, dirPath)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
