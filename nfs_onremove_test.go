package nfs_test

import (
	"os"
	"testing"

	"github.com/willscott/go-nfs/helpers/memfs"
)

// REMOVE and RMDIR name an entry in a directory. Neither may resolve the
// symlink that entry might be: an NFSv3 client walks symlinks itself, one
// LOOKUP at a time, so a server that follows one has acted on an object
// the client never named.
//
// The regression that prompted these: REMOVE stat'ed the target through
// the link, so a DANGLING symlink — every link whose target was already
// deleted, which is most of them once `rm -rf` reaches a directory in
// sorted order — answered NFS3ERR_NOENT and was never removed. rm reads
// that as "already gone" and moves on, the link survives, and the rmdir
// that follows fails with NFS3ERR_NOTEMPTY. Retrying does not converge:
// the same links survive every pass.

func TestRemoveDanglingSymlink(t *testing.T) {
	mem := memfs.New()
	if err := mem.MkdirAll("/d", 0755); err != nil {
		t.Fatal(err)
	}
	if err := mem.Symlink("/d/gone.txt", "/d/link"); err != nil {
		t.Fatal(err)
	}

	target := serveTarget(t, mem)
	if err := target.Remove("/d/link"); err != nil {
		t.Fatalf("REMOVE of a dangling symlink: %v", err)
	}
	if _, err := mem.Lstat("/d/link"); !os.IsNotExist(err) {
		t.Fatalf("the dangling symlink survived REMOVE (Lstat: %v)", err)
	}
}

func TestRemoveSymlinkKeepsItsTarget(t *testing.T) {
	mem := memfs.New()
	if err := mem.MkdirAll("/d", 0755); err != nil {
		t.Fatal(err)
	}
	f, err := mem.Create("/d/target.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("the target must survive")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := mem.Symlink("target.txt", "/d/link"); err != nil {
		t.Fatal(err)
	}

	target := serveTarget(t, mem)
	if err := target.Remove("/d/link"); err != nil {
		t.Fatalf("REMOVE of a live symlink: %v", err)
	}
	if _, err := mem.Lstat("/d/link"); !os.IsNotExist(err) {
		t.Fatalf("the symlink survived REMOVE (Lstat: %v)", err)
	}
	if _, err := mem.Lstat("/d/target.txt"); err != nil {
		t.Fatalf("REMOVE of a symlink deleted its TARGET: %v", err)
	}
}

func TestRemoveSymlinkToDirectory(t *testing.T) {
	mem := memfs.New()
	if err := mem.MkdirAll("/d/real", 0755); err != nil {
		t.Fatal(err)
	}
	if err := mem.Symlink("/d/real", "/d/link"); err != nil {
		t.Fatal(err)
	}

	target := serveTarget(t, mem)
	// REMOVE, not RMDIR: the entry is a symlink whatever it points at, and
	// answering NFS3ERR_ISDIR to this is what following produced.
	if err := target.Remove("/d/link"); err != nil {
		t.Fatalf("REMOVE of a symlink to a directory: %v", err)
	}
	if _, err := mem.Lstat("/d/link"); !os.IsNotExist(err) {
		t.Fatalf("the symlink survived REMOVE (Lstat: %v)", err)
	}
	if _, err := mem.Lstat("/d/real"); err != nil {
		t.Fatalf("REMOVE of a symlink deleted the DIRECTORY it named: %v", err)
	}
}

func TestRmdirRefusesASymlinkToADirectory(t *testing.T) {
	mem := memfs.New()
	if err := mem.MkdirAll("/d/real", 0755); err != nil {
		t.Fatal(err)
	}
	if err := mem.Symlink("/d/real", "/d/link"); err != nil {
		t.Fatal(err)
	}

	target := serveTarget(t, mem)
	if err := target.RmDir("/d/link"); err == nil {
		t.Fatal("RMDIR of a symlink succeeded; it names a symlink, not a directory")
	}
	if _, err := mem.Lstat("/d/real"); err != nil {
		t.Fatalf("RMDIR of a symlink removed the directory it named: %v", err)
	}
	if _, err := mem.Lstat("/d/link"); err != nil {
		t.Fatalf("RMDIR of a symlink removed the symlink: %v", err)
	}
}
