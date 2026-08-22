package nfs_test

import (
	"os"
	"path"
	"testing"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
	"github.com/willscott/go-nfs/helpers/memfs"
)

// ACCESS decides an NFSv3 client's open(2): there is no OPEN in the
// protocol, so a client answers open locally from this reply. A server
// that echoes the requested mask therefore reports "writable" for a file
// it will refuse to write, and the refusal surfaces at the first WRITE
// rather than at open.
//
// These pin the reply against the granted permission triple, including
// the parts that do not follow from the triple alone -- a directory's
// MODIFY costs search as well as write, DELETE belongs to a directory and
// never to a file -- which is where knfsd's access maps
// (fs/nfsd/nfs3proc.c) differ from the obvious mapping.

// permFS answers Permitted from a table, so a case names the permission
// decision it is testing rather than arranging a uid to produce it.
type permFS struct {
	billy.Filesystem
	perm map[string]nfs.Permission
	err  error
}

var _ nfs.PermissionChecker = (*permFS)(nil)

func (p *permFS) Permitted(name string) (nfs.Permission, error) {
	if p.err != nil {
		return 0, p.err
	}
	// The handler names the object the way the filesystem does, which for
	// an object directly under the export root is a bare name.
	return p.perm[path.Clean("/"+name)], nil
}

const allAccess = nfs.AccessRead | nfs.AccessLookup | nfs.AccessModify |
	nfs.AccessExtend | nfs.AccessDelete | nfs.AccessExecute

func accessTree(t *testing.T) billy.Filesystem {
	t.Helper()
	mem := memfs.New()
	if err := mem.MkdirAll("/searchable", 0755); err != nil {
		t.Fatal(err)
	}
	if err := mem.MkdirAll("/unsearchable", 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"/readonly.txt", "/writable.txt", "/program"} {
		f, err := mem.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("contents")); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return mem
}

func TestAccessAnswersFromTheFilesystem(t *testing.T) {
	fs := &permFS{Filesystem: accessTree(t), perm: map[string]nfs.Permission{
		"/readonly.txt": nfs.PermissionRead,
		"/writable.txt": nfs.PermissionRead | nfs.PermissionWrite,
		"/program":      nfs.PermissionRead | nfs.PermissionExecute,
		"/searchable":   nfs.PermissionRead | nfs.PermissionWrite | nfs.PermissionExecute,
		// Write without search: nothing in the directory can be named, so
		// nothing in it can be changed either.
		"/unsearchable": nfs.PermissionRead | nfs.PermissionWrite,
	}}
	target := serveTarget(t, fs)

	for _, tc := range []struct {
		name string
		path string
		ask  uint32
		want uint32
	}{
		{
			name: "a read-only file is not modifiable",
			path: "/readonly.txt",
			ask:  allAccess,
			want: nfs.AccessRead,
		},
		{
			name: "a writable file extends and modifies, and is still not deletable",
			path: "/writable.txt",
			ask:  allAccess,
			want: nfs.AccessRead | nfs.AccessModify | nfs.AccessExtend,
		},
		{
			name: "an executable file",
			path: "/program",
			ask:  allAccess,
			want: nfs.AccessRead | nfs.AccessExecute,
		},
		{
			name: "a searchable, writable directory",
			path: "/searchable",
			ask:  allAccess,
			want: nfs.AccessRead | nfs.AccessLookup | nfs.AccessModify |
				nfs.AccessExtend | nfs.AccessDelete,
		},
		{
			name: "a directory without search grants neither lookup nor any change",
			path: "/unsearchable",
			ask:  allAccess,
			want: nfs.AccessRead,
		},
		{
			name: "only the bits the client asked about come back",
			path: "/writable.txt",
			ask:  nfs.AccessModify,
			want: nfs.AccessModify,
		},
		{
			name: "asking about a denied bit alone is answered with nothing",
			path: "/readonly.txt",
			ask:  nfs.AccessModify | nfs.AccessExtend,
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := target.Access(tc.path, tc.ask)
			if err != nil {
				t.Fatalf("ACCESS(%s): %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("ACCESS(%s, %#x) = %#x, want %#x", tc.path, tc.ask, got, tc.want)
			}
		})
	}
}

// A permission error is the answer, not a failure: knfsd's ACCESS loop
// keeps going past EACCES and reports the bit as not granted, because
// reporting what is denied is the entire purpose of the procedure.
func TestAccessReportsARefusalRatherThanFailing(t *testing.T) {
	fs := &permFS{Filesystem: accessTree(t), err: os.ErrPermission}
	target := serveTarget(t, fs)

	got, err := target.Access("/readonly.txt", allAccess)
	if err != nil {
		t.Fatalf("ACCESS: %v", err)
	}
	if got != 0 {
		t.Errorf("ACCESS = %#x for an object the filesystem refuses entirely, want 0", got)
	}
}

// A filesystem with no permission model of its own keeps the historical
// reply. Enforcing on its behalf would deny operations it would have
// allowed.
func TestAccessWithoutACheckerEchoesTheMask(t *testing.T) {
	target := serveTarget(t, accessTree(t))

	got, err := target.Access("/readonly.txt", allAccess)
	if err != nil {
		t.Fatalf("ACCESS: %v", err)
	}
	if got != allAccess {
		t.Errorf("ACCESS = %#x, want the mask %#x echoed back", got, allAccess)
	}
}
