package nfs_test

import (
	"bytes"
	"io"
	"net"
	"os"
	"testing"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/helpers"
	"github.com/willscott/go-nfs/helpers/memfs"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// linkArgs is LINK3args as RFC 1813 defines it: `nfs_fh3 file` followed by
// `diropargs3 link`. The client library has no LINK call, so the test
// speaks the procedure itself.
type linkArgs struct {
	rpc.Header
	File []byte
	Dir  []byte
	Name []byte
}

// hardlinkFS gives an in-memory filesystem the one operation LINK needs.
// It records the arguments so a test can assert which paths the handler
// derived from the two file handles in the request.
type hardlinkFS struct {
	billy.Filesystem
	old, new string
}

var _ nfs.HardLinker = (*hardlinkFS)(nil)

func (h *hardlinkFS) Link(oldname, newname string) error {
	src, err := h.Filesystem.Open(oldname)
	if err != nil {
		return err
	}
	defer src.Close()
	content, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	dst, err := h.Filesystem.Create(newname)
	if err != nil {
		return err
	}
	if _, err := dst.Write(content); err != nil {
		dst.Close()
		return err
	}
	h.old, h.new = oldname, newname
	return dst.Close()
}

// serveTarget brings up a server over fs and mounts it.
func serveTarget(t *testing.T, fs billy.Filesystem) *nfsc.Target {
	t.Helper()

	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		_ = nfs.Serve(listener, helpers.NewCachingHandler(helpers.NewNullAuthHandler(fs), 1024))
	}()

	c, err := rpc.DialTCP(listener.Addr().Network(), listener.Addr().(*net.TCPAddr).String(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	var mounter nfsc.Mount
	mounter.Client = c
	target, err := mounter.Mount("/", rpc.AuthNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounter.Unmount() })
	return target
}

func linkCall(t *testing.T, target *nfsc.Target, file, dir []byte, name string) io.ReadSeeker {
	t.Helper()
	res, err := target.Call(&linkArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    nfsc.Nfs3Prog,
			Vers:    nfsc.Nfs3Vers,
			Proc:    uint32(nfs.NFSProcedureLink),
			Cred:    rpc.AuthNull,
			Verf:    rpc.AuthNull,
		},
		File: file,
		Dir:  dir,
		Name: []byte(name),
	})
	if err != nil {
		t.Fatalf("LINK rpc: %v", err)
	}
	return res
}

func TestLink(t *testing.T) {
	mem := &hardlinkFS{Filesystem: memfs.New()}
	content := []byte("linked content")
	f, err := mem.Create("/orig.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	f.Close()

	target := serveTarget(t, mem)
	dirHandle, err := target.Mkdir("/d", 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, fileHandle, err := target.Lookup("/orig.txt")
	if err != nil {
		t.Fatal(err)
	}

	res := linkCall(t, target, fileHandle, dirHandle, "link.txt")

	status, err := xdr.ReadUint32(res)
	if err != nil {
		t.Fatal(err)
	}
	if status != uint32(nfs.NFSStatusOk) {
		t.Fatalf("LINK status = %d, want 0", status)
	}
	// LINK3resok is the file's post_op_attr followed by the directory's
	// wcc_data -- and no file handle, which a client would desynchronize on.
	follows, err := xdr.ReadUint32(res)
	if err != nil || follows != 1 {
		t.Fatalf("post_op_attr follows = %d (err %v), want 1", follows, err)
	}
	var attrs nfs.FileAttribute
	if err := xdr.Read(res, &attrs); err != nil {
		t.Fatalf("decoding the file attributes: %v", err)
	}
	if attrs.Filesize != uint64(len(content)) {
		t.Fatalf("linked file size = %d, want %d", attrs.Filesize, len(content))
	}

	// The two handles must have resolved to the source file and the target
	// directory; parsing the request as anything else crosses them over.
	if mem.old != "orig.txt" || mem.new != "d/link.txt" {
		t.Fatalf("Link(%q, %q), want Link(\"orig.txt\", \"d/link.txt\")", mem.old, mem.new)
	}
	got, err := readFile(mem, "d/link.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("link content = %q, want %q", got, content)
	}
}

// TestLinkFailureIsDecodable pins the size of LINK3resfail. A body shorter
// than post_op_attr + wcc_data leaves a client unable to decode the
// rejection at all, which it reports as EIO instead of the status the
// server chose.
func TestLinkFailureIsDecodable(t *testing.T) {
	mem := &hardlinkFS{Filesystem: memfs.New()}
	for _, name := range []string{"/orig.txt", "/taken.txt"} {
		f, err := mem.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	target := serveTarget(t, mem)
	_, rootHandle, err := target.Lookup("/")
	if err != nil {
		t.Fatal(err)
	}
	_, fileHandle, err := target.Lookup("/orig.txt")
	if err != nil {
		t.Fatal(err)
	}

	res := linkCall(t, target, fileHandle, rootHandle, "taken.txt")

	status, err := xdr.ReadUint32(res)
	if err != nil {
		t.Fatal(err)
	}
	if status != uint32(nfs.NFSStatusExist) {
		t.Fatalf("LINK status = %d, want %d (NFS3ERR_EXIST)", status, nfs.NFSStatusExist)
	}
	// post_op_attr "absent", then wcc_data's two "absent" flags.
	for i := 0; i < 3; i++ {
		v, err := xdr.ReadUint32(res)
		if err != nil {
			t.Fatalf("LINK3resfail is truncated at word %d: %v", i, err)
		}
		if v != 0 {
			t.Fatalf("LINK3resfail word %d = %d, want 0", i, v)
		}
	}
	if _, err := xdr.ReadUint32(res); err == nil {
		t.Fatal("LINK3resfail carries trailing bytes")
	}
}

func readFile(fs billy.Filesystem, name string) ([]byte, error) {
	f, err := fs.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
