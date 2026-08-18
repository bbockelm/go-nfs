package nfs

import (
	"bytes"
	"context"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

const (
	createModeUnchecked = 0
	createModeGuarded   = 1
	createModeExclusive = 2
)

func onCreate(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = wccDataErrorFormatter
	obj := DirOpArg{}
	err := xdr.Read(w.req.Body, &obj)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	how, err := xdr.ReadUint32(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	var attrs *SetFileAttributes
	var exclusiveVerf [8]byte
	if how == createModeUnchecked || how == createModeGuarded {
		sattr, err := ReadSetFileAttributes(w.req.Body)
		if err != nil {
			return &NFSStatusError{NFSStatusInval, err}
		}
		attrs = sattr
	} else if how == createModeExclusive {
		// read createverf3
		if err := xdr.Read(w.req.Body, &exclusiveVerf); err != nil {
			return &NFSStatusError{NFSStatusInval, err}
		}
		// EXCLUSIVE carries a verifier where the other modes carry
		// attributes; Apply dereferences its receiver's fields, so it gets
		// an empty set rather than nil. The client follows with SETATTR.
		attrs = &SetFileAttributes{}
	} else {
		// invalid
		return &NFSStatusError{NFSStatusNotSupp, os.ErrInvalid}
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

	newFile := append(path, string(obj.Filename))
	newFilePath := fs.Join(newFile...)
	// retransmit is set when an EXCLUSIVE create repeats a verifier already
	// honored for this name; the file must then be left exactly as it is.
	retransmit := false
	if s, err := fs.Stat(newFilePath); err == nil {
		if s.IsDir() {
			return &NFSStatusError{NFSStatusExist, nil}
		}
		if how == createModeGuarded {
			return &NFSStatusError{NFSStatusExist, os.ErrPermission}
		}
		if how == createModeExclusive {
			// RFC 1813: repeating the verifier of a create that already
			// succeeded is a retransmission and succeeds idempotently. Any
			// other verifier means the name is genuinely taken.
			if !exclusiveCreates.matches(verifierKey(fs, newFilePath), exclusiveVerf) {
				return &NFSStatusError{NFSStatusExist, os.ErrExist}
			}
			retransmit = true
		}
	} else {
		if s, err := fs.Stat(fs.Join(path...)); err != nil {
			return &NFSStatusError{NFSStatusAccess, err}
		} else if !s.IsDir() {
			return &NFSStatusError{NFSStatusNotDir, nil}
		}
	}

	// fs.Create truncates, so it must not run for a retransmission: the
	// original request's data would be discarded.
	if !retransmit {
		file, err := fs.Create(newFilePath)
		if err != nil {
			Log.Errorf("Error Creating: %v", err)
			return &NFSStatusError{NFSStatusAccess, err}
		}
		if err := file.Close(); err != nil {
			Log.Errorf("Error Creating: %v", err)
			return &NFSStatusError{NFSStatusAccess, err}
		}
		if how == createModeExclusive {
			exclusiveCreates.remember(verifierKey(fs, newFilePath), exclusiveVerf)
		}
	}

	fp := userHandle.ToHandle(fs, newFile)
	changer := userHandle.Change(fs)
	if err := attrs.Apply(changer, fs, newFilePath); err != nil {
		Log.Errorf("Error applying attributes: %v\n", err)
		return &NFSStatusError{NFSStatusIO, err}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	// "handle follows"
	if err := xdr.Write(writer, uint32(1)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := xdr.Write(writer, fp); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, newFile)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	// dir_wcc (we don't include pre_op_attr)
	if err := xdr.Write(writer, uint32(0)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
