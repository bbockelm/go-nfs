package nfs_test

import (
	"bytes"
	"testing"

	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/helpers/memfs"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// exclusiveCreateArgs is CREATE3args in EXCLUSIVE mode: diropargs3, the
// createhow3 discriminant, and a createverf3 in place of the sattr3 the
// other two modes carry. The client library only sends GUARDED, so the
// test speaks the procedure itself.
type exclusiveCreateArgs struct {
	rpc.Header
	Dir  []byte
	Name []byte
	How  uint32
	Verf [8]byte
}

const createModeExclusive = 2

func exclusiveCreate(t *testing.T, target *nfsc.Target, dir []byte, name string, verf [8]byte) uint32 {
	t.Helper()
	res, err := target.Call(&exclusiveCreateArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    nfsc.Nfs3Prog,
			Vers:    nfsc.Nfs3Vers,
			Proc:    uint32(nfs.NFSProcedureCreate),
			Cred:    rpc.AuthNull,
			Verf:    rpc.AuthNull,
		},
		Dir:  dir,
		Name: []byte(name),
		How:  createModeExclusive,
		Verf: verf,
	})
	if err != nil {
		t.Fatalf("CREATE rpc: %v", err)
	}
	status, err := xdr.ReadUint32(res)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// TestExclusiveCreate covers the three answers EXCLUSIVE mode has: create
// when the name is free, succeed without touching anything when the same
// verifier comes back (a retransmission), and refuse when a different one
// does (the name is genuinely taken).
func TestExclusiveCreate(t *testing.T) {
	mem := memfs.New()
	// memfs only acknowledges the root once something is in it.
	if f, err := mem.Create("/anchor"); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	target := serveTarget(t, mem)
	_, rootHandle, err := target.Lookup("/")
	if err != nil {
		t.Fatal(err)
	}

	mine := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	theirs := [8]byte{8, 7, 6, 5, 4, 3, 2, 1}

	if status := exclusiveCreate(t, target, rootHandle, "excl.txt", mine); status != uint32(nfs.NFSStatusOk) {
		t.Fatalf("exclusive create of a free name = %d, want 0", status)
	}
	if _, err := mem.Stat("/excl.txt"); err != nil {
		t.Fatalf("exclusive create did not create the file: %v", err)
	}

	// The client writes before it retransmits; the retry must not truncate
	// what the original request's data already put there.
	content := []byte("written after the create")
	f, err := mem.OpenFile("/excl.txt", 0o2 /* O_RDWR */, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if status := exclusiveCreate(t, target, rootHandle, "excl.txt", mine); status != uint32(nfs.NFSStatusOk) {
		t.Fatalf("retransmitted exclusive create = %d, want 0", status)
	}
	got, err := readFile(mem, "/excl.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("retransmission truncated the file: %q, want %q", got, content)
	}

	if status := exclusiveCreate(t, target, rootHandle, "excl.txt", theirs); status != uint32(nfs.NFSStatusExist) {
		t.Fatalf("exclusive create of a taken name = %d, want %d (NFS3ERR_EXIST)", status, nfs.NFSStatusExist)
	}

	// Removing the name releases it, verifier and all.
	if err := target.Remove("/excl.txt"); err != nil {
		t.Fatal(err)
	}
	if status := exclusiveCreate(t, target, rootHandle, "excl.txt", theirs); status != uint32(nfs.NFSStatusOk) {
		t.Fatalf("exclusive create after remove = %d, want 0", status)
	}
}
