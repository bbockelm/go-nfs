package nfs_test

import (
	"bytes"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/helpers/memfs"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// A filesystem that buffers has to be asked before COMMIT may answer OK,
// and -- the half that is easy to miss -- before WRITE may claim
// FILE_SYNC. A client only commits what the server called UNSTABLE, so a
// server that overclaims on the WRITE reply is never sent a COMMIT at all
// and fsync(2) on the mount becomes a no-op that returns success.

// writeArgs is WRITE3args (RFC 1813, 3.3.7). The client library's Write
// does not let a test choose the stability, so the test speaks the
// procedure itself.
type writeArgs struct {
	rpc.Header
	Handle []byte
	Offset uint64
	Count  uint32
	How    uint32
	Data   []byte
}

// commitArgs is COMMIT3args (RFC 1813, 3.3.21).
type commitArgs struct {
	rpc.Header
	Handle []byte
	Offset uint64
	Count  uint32
}

// committingFS is an in-memory filesystem that can be told to commit, and
// counts how often it is.
type committingFS struct {
	billy.Filesystem
	commits []string
	fail    error
}

var _ nfs.Committer = (*committingFS)(nil)

func (c *committingFS) Commit(path string) error {
	c.commits = append(c.commits, path)
	return c.fail
}

func header(proc nfs.NFSProcedure) rpc.Header {
	return rpc.Header{
		Rpcvers: 2,
		Prog:    nfsc.Nfs3Prog,
		Vers:    nfsc.Nfs3Vers,
		Proc:    uint32(proc),
		Cred:    rpc.AuthNull,
		Verf:    rpc.AuthNull,
	}
}

// writeCall issues one WRITE and returns the status, the stability the
// reply claimed, and the write verifier. The reply is
// `status, wcc_data, count, committed, verf`, and wcc_data is variable
// length, so the three fixed trailing words are read from the end.
func writeCall(t *testing.T, target *nfsc.Target, handle []byte, off uint64, data []byte, how uint32) (status, committed uint32, verf [8]byte) {
	t.Helper()
	res, err := target.Call(&writeArgs{
		Header: header(nfs.NFSProcedureWrite),
		Handle: handle,
		Offset: off,
		Count:  uint32(len(data)),
		How:    how,
		Data:   data,
	})
	if err != nil {
		t.Fatalf("WRITE rpc: %v", err)
	}
	status, err = xdr.ReadUint32(res)
	if err != nil {
		t.Fatal(err)
	}
	if status != uint32(nfs.NFSStatusOk) {
		return status, 0, verf
	}
	tail := readTail(t, res, 16)
	committed = be32(tail[4:8])
	copy(verf[:], tail[8:])
	return status, committed, verf
}

// commitCall issues one COMMIT and returns the status and the verifier.
// The reply is `status, wcc_data, verf`.
func commitCall(t *testing.T, target *nfsc.Target, handle []byte) (status uint32, verf [8]byte) {
	t.Helper()
	res, err := target.Call(&commitArgs{
		Header: header(nfs.NFSProcedureCommit),
		Handle: handle,
		Offset: 0,
		Count:  0,
	})
	if err != nil {
		t.Fatalf("COMMIT rpc: %v", err)
	}
	status, err = xdr.ReadUint32(res)
	if err != nil {
		t.Fatal(err)
	}
	if status != uint32(nfs.NFSStatusOk) {
		return status, verf
	}
	copy(verf[:], readTail(t, res, 8))
	return status, verf
}

func readTail(t *testing.T, res io.ReadSeeker, n int64) []byte {
	t.Helper()
	if _, err := res.Seek(-n, io.SeekEnd); err != nil {
		t.Fatalf("seeking to the last %d bytes of the reply: %v", n, err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(res, buf); err != nil {
		t.Fatalf("reading the last %d bytes of the reply: %v", n, err)
	}
	return buf
}

func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

const (
	stableUnstable = 0
	stableDataSync = 1
	stableFileSync = 2
)

func fileHandle(t *testing.T, target *nfsc.Target, path string) []byte {
	t.Helper()
	_, handle, err := target.Lookup(path)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func createFile(t *testing.T, fs billy.Filesystem, path string) {
	t.Helper()
	f, err := fs.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// THE ONE THAT MATTERS. An unstable write to a filesystem that buffers
// must be answered UNSTABLE, or the client never asks for a commit.
func TestAnUnstableWriteIsNotClaimedStable(t *testing.T) {
	fs := &committingFS{Filesystem: memfs.New()}
	createFile(t, fs, "/buffered.txt")
	target := serveTarget(t, fs)
	handle := fileHandle(t, target, "/buffered.txt")

	status, committed, _ := writeCall(t, target, handle, 0, []byte("unstable"), stableUnstable)
	if status != uint32(nfs.NFSStatusOk) {
		t.Fatalf("WRITE status %d", status)
	}
	if committed != stableUnstable {
		t.Fatalf("an unstable write to a buffering filesystem was answered with stability %d; "+
			"a client believes that reply and will never send COMMIT, so fsync(2) on the mount "+
			"makes nothing durable", committed)
	}
	if len(fs.commits) != 0 {
		t.Fatalf("an unstable write committed anyway: %v", fs.commits)
	}
}

// A client that asked for a synchronous write is entitled to one, so the
// commit happens before the reply rather than being deferred.
func TestASynchronousWriteCommitsBeforeItIsAnswered(t *testing.T) {
	for _, how := range []uint32{stableDataSync, stableFileSync} {
		fs := &committingFS{Filesystem: memfs.New()}
		createFile(t, fs, "/sync.txt")
		target := serveTarget(t, fs)
		handle := fileHandle(t, target, "/sync.txt")

		status, committed, _ := writeCall(t, target, handle, 0, []byte("synchronous"), how)
		if status != uint32(nfs.NFSStatusOk) {
			t.Fatalf("how=%d: WRITE status %d", how, status)
		}
		if committed != stableFileSync {
			t.Errorf("how=%d: committed %d, want FILE_SYNC after a commit", how, committed)
		}
		// The path is the export-relative one every other handler
		// derives from a file handle (fs.Join of the handle's
		// components), not an absolute path in the server's namespace.
		if len(fs.commits) != 1 || fs.commits[0] != "sync.txt" {
			t.Errorf("how=%d: commits %v, want one for sync.txt", how, fs.commits)
		}
	}
}

// A filesystem that writes through keeps the historical reply: nothing is
// buffered, so FILE_SYNC is what it achieved and a client saving itself a
// COMMIT round trip is right to.
func TestAWriteThroughFilesystemStillClaimsFileSync(t *testing.T) {
	fs := memfs.New()
	createFile(t, fs, "/plain.txt")
	target := serveTarget(t, fs)
	handle := fileHandle(t, target, "/plain.txt")

	status, committed, _ := writeCall(t, target, handle, 0, []byte("through"), stableUnstable)
	if status != uint32(nfs.NFSStatusOk) {
		t.Fatalf("WRITE status %d", status)
	}
	if committed != stableFileSync {
		t.Fatalf("committed %d, want FILE_SYNC for a filesystem that does not implement Committer", committed)
	}
}

func TestCommitReachesTheFilesystem(t *testing.T) {
	fs := &committingFS{Filesystem: memfs.New()}
	createFile(t, fs, "/buffered.txt")
	target := serveTarget(t, fs)
	handle := fileHandle(t, target, "/buffered.txt")

	if _, _, _ = writeCall(t, target, handle, 0, []byte("unstable"), stableUnstable); len(fs.commits) != 0 {
		t.Fatalf("the write committed: %v", fs.commits)
	}
	status, _ := commitCall(t, target, handle)
	if status != uint32(nfs.NFSStatusOk) {
		t.Fatalf("COMMIT status %d", status)
	}
	if len(fs.commits) != 1 || fs.commits[0] != "buffered.txt" {
		t.Fatalf("commits %v, want one for buffered.txt", fs.commits)
	}
}

// A COMMIT the filesystem could not honor is a failure, not an OK. The
// whole point of the procedure is that its answer can be believed.
func TestCommitReportsAFailureFromTheFilesystem(t *testing.T) {
	fs := &committingFS{Filesystem: memfs.New(), fail: syscall.ENOSPC}
	createFile(t, fs, "/buffered.txt")
	target := serveTarget(t, fs)
	handle := fileHandle(t, target, "/buffered.txt")

	status, _ := commitCall(t, target, handle)
	if status != uint32(nfs.NFSStatusNoSPC) {
		t.Fatalf("COMMIT over a filesystem that could not commit answered %d, want NFS3ERR_NOSPC (%d)",
			status, nfs.NFSStatusNoSPC)
	}
}

func TestCommitOnAWriteThroughFilesystemSucceeds(t *testing.T) {
	fs := memfs.New()
	createFile(t, fs, "/plain.txt")
	target := serveTarget(t, fs)
	handle := fileHandle(t, target, "/plain.txt")

	if status, _ := commitCall(t, target, handle); status != uint32(nfs.NFSStatusOk) {
		t.Fatalf("COMMIT status %d, want OK", status)
	}
}

// The write verifier is how a client learns that the instance holding its
// unstable writes is gone. It has to be the same for WRITE and COMMIT
// within one server, and different across servers.
func TestTheWriteVerifierIsPerServerAndConsistent(t *testing.T) {
	fs := &committingFS{Filesystem: memfs.New()}
	createFile(t, fs, "/v.txt")
	target := serveTarget(t, fs)
	handle := fileHandle(t, target, "/v.txt")

	_, _, wverf := writeCall(t, target, handle, 0, []byte("v"), stableUnstable)
	_, cverf := commitCall(t, target, handle)
	if wverf != cverf {
		t.Fatalf("WRITE verifier %x != COMMIT verifier %x; a client compares the two and would "+
			"resend every unstable write it had made", wverf, cverf)
	}
	if wverf == ([8]byte{}) {
		t.Fatal("the write verifier is all zeroes, so a restart is indistinguishable from a running server")
	}

	other := &committingFS{Filesystem: memfs.New()}
	createFile(t, other, "/v.txt")
	target2 := serveTarget(t, other)
	_, _, wverf2 := writeCall(t, target2, fileHandle(t, target2, "/v.txt"), 0, []byte("v"), stableUnstable)
	if wverf == wverf2 {
		t.Fatal("two server instances minted the same write verifier, so a client cannot tell that " +
			"the server it sent unstable writes to has restarted")
	}
}

// The bytes still have to arrive, whichever stability was asked for.
func TestWritesLandRegardlessOfStability(t *testing.T) {
	fs := &committingFS{Filesystem: memfs.New()}
	createFile(t, fs, "/data.txt")
	target := serveTarget(t, fs)
	handle := fileHandle(t, target, "/data.txt")

	want := []byte("the bytes must be there")
	if status, _, _ := writeCall(t, target, handle, 0, want, stableUnstable); status != uint32(nfs.NFSStatusOk) {
		t.Fatalf("WRITE status %d", status)
	}
	f, err := fs.Open("/data.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil && err != os.ErrClosed {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read back %q, want %q", got, want)
	}
}
