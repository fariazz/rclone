//go:build unix

package nfs

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"math"
	"path"
	"strings"
	"sync"

	billy "github.com/go-git/go-billy/v5"
)

// stableCache retains identities for the life of a server. NFSv3 has no OPEN
// or CLOSE messages, so evicting handles could invalidate a client's open file.
// Names are updated by FS mutations, before go-nfs calls InvalidateHandle.
type stableCache struct {
	namespace sync.RWMutex
	mu        sync.Mutex
	root      *FS
	prefix    [8]byte
	next      uint64
	names     map[string]string
	handles   map[string]string
}

func newStableCache(root *FS) *stableCache {
	c := &stableCache{root: root, names: make(map[string]string), handles: make(map[string]string)}
	_, _ = rand.Read(c.prefix[:])
	root.stable = c
	return c
}

func stablePath(name string) string { return strings.TrimPrefix(path.Clean("/"+name), "/") }

// handle returns a copy so callers cannot mutate the stored identity.
func (c *stableCache) handle(name string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	name = stablePath(name)
	if h, ok := c.names[name]; ok {
		return []byte(h)
	}
	c.next++
	h := make([]byte, 16)
	copy(h, c.prefix[:])
	binary.BigEndian.PutUint64(h[8:], c.next)
	c.names[name] = string(h)
	c.handles[string(h)] = name
	return h
}

// ToHandle returns a process-lifetime identity for a filesystem name.
func (c *stableCache) ToHandle(_ billy.Filesystem, parts []string) []byte {
	c.namespace.RLock()
	defer c.namespace.RUnlock()
	name, err := c.resolveName(path.Join(parts...))
	if err != nil {
		return nil
	}
	return c.handle(name)
}

// FromHandle returns an opaque path resolved when the filesystem operation runs.
func (c *stableCache) FromHandle(h []byte) (billy.Filesystem, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.handles[string(h)]
	if !ok {
		return nil, nil, errStaleHandle
	}
	return c.root, []string{handlePathPrefix + hex.EncodeToString(h)}, nil
}

// InvalidateHandle is a no-op: FS.Remove and FS.Rename update identities as
// part of the mutation. go-nfs also calls this after a successful rename,
// when invalidating the source handle would break open files.
func (c *stableCache) InvalidateHandle(_ billy.Filesystem, _ []byte) error { return nil }

// HandleLimit reports that live identities are never evicted.
func (c *stableCache) HandleLimit() int { return math.MaxInt }

func pathWithin(name, parent string) bool {
	return name == parent || strings.HasPrefix(name, parent+"/")
}

// rename serializes namespace mutation and handle updates against lookups.
func (c *stableCache) rename(oldname, newname string, mutate func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := mutate(); err != nil {
		return err
	}
	oldname, newname = stablePath(oldname), stablePath(newname)
	if oldname == newname {
		return nil
	}
	moved := make(map[string]string)
	for name, h := range c.names {
		switch {
		case pathWithin(name, oldname):
			moved[newname+strings.TrimPrefix(name, oldname)] = h
			delete(c.names, name)
		case pathWithin(name, newname):
			delete(c.names, name)
			delete(c.handles, h)
		}
	}
	for name, h := range moved {
		c.names[name] = h
		c.handles[h] = name
	}
	return nil
}

func (c *stableCache) remove(name string, mutate func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := mutate(); err != nil {
		return err
	}
	name = stablePath(name)
	for entry, h := range c.names {
		if pathWithin(entry, name) {
			delete(c.names, entry)
			delete(c.handles, h)
		}
	}
	return nil
}

// Handle paths are resolved at the filesystem operation, under the namespace
// lock. Returning a plain name from FromHandle would let a concurrent rename
// and name reuse redirect a WRITE to an unrelated file.
const handlePathPrefix = "\x00nfs:"

func (c *stableCache) resolveName(name string) (string, error) {
	first, rest, _ := strings.Cut(name, "/")
	if !strings.HasPrefix(first, handlePathPrefix) {
		return name, nil
	}
	h, err := hex.DecodeString(strings.TrimPrefix(first, handlePathPrefix))
	if err != nil {
		return "", errStaleHandle
	}
	c.mu.Lock()
	target, ok := c.handles[string(h)]
	c.mu.Unlock()
	if !ok {
		return "", errStaleHandle
	}
	return path.Join(target, rest), nil
}
