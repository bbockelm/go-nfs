package nfs

import (
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
)

// exclusiveCreateTTL bounds how long the verifier of an EXCLUSIVE create is
// remembered. It only has to outlive a client's retransmission window: the
// record exists so a retried CREATE for a request that already succeeded is
// answered idempotently instead of with NFS3ERR_EXIST.
const exclusiveCreateTTL = 2 * time.Minute

// exclusiveCreateMax bounds the map. Expired records are swept when it is
// reached, and the sweep is the only thing that removes them in bulk.
const exclusiveCreateMax = 4096

// exclusiveCreates remembers the verifiers of recent EXCLUSIVE creates.
//
// RFC 1813 has the server persist the verifier in the file's metadata, and
// classic implementations overload atime/mtime to do it. That corrupts
// timestamps and needs the backing store's cooperation, so this keeps the
// association in memory instead. The guarantee that matters -- never
// truncating a file whose name was won by someone else -- holds either way;
// a verifier lost to a restart or to the TTL merely turns a retransmission
// back into the EXIST error it would have been before.
var exclusiveCreates = &verifierCache{entries: make(map[string]verifierEntry)}

type verifierEntry struct {
	verf    [8]byte
	expires time.Time
}

type verifierCache struct {
	mu      sync.Mutex
	entries map[string]verifierEntry
}

// verifierKey names a file across the filesystems one server can export.
// Root is what distinguishes them; two exports sharing a root string are
// assumed to be the same tree.
func verifierKey(fs billy.Filesystem, path string) string {
	return fs.Root() + "\x00" + path
}

func (c *verifierCache) remember(key string, verf [8]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.entries) >= exclusiveCreateMax {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = verifierEntry{verf: verf, expires: now.Add(exclusiveCreateTTL)}
}

// matches reports whether this exact verifier created the file and the
// record is still current -- that is, whether the request is a
// retransmission rather than a collision with a name someone else took.
func (c *verifierCache) matches(key string, verf [8]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return false
	}
	if time.Now().After(e.expires) {
		delete(c.entries, key)
		return false
	}
	return e.verf == verf
}

// forget drops any verifier recorded for a name that is being released, so
// the next creator of that name starts from nothing.
func (c *verifierCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}
