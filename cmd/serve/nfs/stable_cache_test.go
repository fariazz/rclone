//go:build unix

package nfs

import (
	"fmt"
	nfsfile "github.com/willscott/go-nfs/file"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

func TestStableHandleRename(t *testing.T) {
	h := newTestHandler(t)
	h.opt.HandleCache = cacheStable
	cache, err := h.getCache()
	require.NoError(t, err)
	h.Cache = cache
	// Follow the go-nfs RENAME RPC sequence, including its invalidation callback.
	handle := h.ToHandle(h.billyFS, []string{"sub", "hello.txt"})
	dirHandle := h.ToHandle(h.billyFS, []string{"sub"})
	require.NoError(t, h.billyFS.Rename("sub/hello.txt", "sub/renamed.txt"))
	require.NoError(t, h.InvalidateHandle(h.billyFS, handle))
	_, p, err := h.FromHandle(handle)
	require.NoError(t, err)
	require.Equal(t, "sub/renamed.txt", resolvedTestPath(t, h, p))
	require.Equal(t, handle, h.ToHandle(h.billyFS, p))
	require.NoError(t, h.billyFS.Rename("sub", "moved"))
	require.NoError(t, h.InvalidateHandle(h.billyFS, dirHandle))
	_, p, err = h.FromHandle(handle)
	require.NoError(t, err)
	require.Equal(t, "moved/renamed.txt", resolvedTestPath(t, h, p))
	f, err := h.billyFS.Open(resolvedTestPath(t, h, p))
	require.NoError(t, err)
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, "world", string(data))
}

func newStableTestHandler(t *testing.T) *Handler {
	t.Helper()
	h := newTestHandler(t)
	h.opt.HandleCache = cacheStable
	cache, err := h.getCache()
	require.NoError(t, err)
	h.Cache = cache
	return h
}

func TestStableHandleReplacementAndRemoval(t *testing.T) {
	h := newStableTestHandler(t)
	original := h.ToHandle(h.billyFS, []string{"sub", "hello.txt"})
	temp, err := h.billyFS.Create("sub/save.tmp")
	require.NoError(t, err)
	_, err = temp.Write([]byte("saved"))
	require.NoError(t, err)
	require.NoError(t, temp.Close())
	replacement := h.ToHandle(h.billyFS, []string{"sub", "save.tmp"})
	require.NoError(t, h.billyFS.Rename("sub/save.tmp", "sub/hello.txt"))
	require.NoError(t, h.InvalidateHandle(h.billyFS, replacement))
	_, _, err = h.FromHandle(original)
	require.ErrorIs(t, err, errStaleHandle, "old handle must never target replacement data")
	require.Equal(t, replacement, h.ToHandle(h.billyFS, []string{"sub", "hello.txt"}))
	require.NoError(t, h.billyFS.Remove("sub/hello.txt"))
	require.NoError(t, h.InvalidateHandle(h.billyFS, replacement))
	_, _, err = h.FromHandle(replacement)
	require.ErrorIs(t, err, errStaleHandle)
	require.NotEqual(t, replacement, h.ToHandle(h.billyFS, []string{"sub", "hello.txt"}))
}

func TestStableHandleFailedRename(t *testing.T) {
	h := newStableTestHandler(t)
	original := h.ToHandle(h.billyFS, []string{"sub", "hello.txt"})
	require.Error(t, h.billyFS.Rename("missing", "sub/hello.txt"))
	_, p, err := h.FromHandle(original)
	require.NoError(t, err)
	require.Equal(t, "sub/hello.txt", resolvedTestPath(t, h, p))
	require.NoError(t, h.billyFS.Rename("sub/hello.txt", "sub/hello.txt"))
	require.NoError(t, h.InvalidateHandle(h.billyFS, original))
	_, _, err = h.FromHandle(original)
	require.NoError(t, err)
}

func TestStableHandleSubpath(t *testing.T) {
	h := newStableTestHandler(t)
	sub := h.billyFS.subFS("/sub")
	original := h.ToHandle(sub, []string{"hello.txt"})
	require.Equal(t, original, h.ToHandle(h.billyFS, []string{"sub", "hello.txt"}))
	require.NoError(t, sub.Rename("hello.txt", "renamed.txt"))
	require.NoError(t, h.InvalidateHandle(sub, original))
	_, p, err := h.FromHandle(original)
	require.NoError(t, err)
	require.Equal(t, "sub/renamed.txt", resolvedTestPath(t, h, p))
}

func TestStableFileIDAfterVFSRefresh(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("persisted"), 0600))
	backend, err := fs.NewFs(t.Context(), dir)
	require.NoError(t, err)
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeOff
	V := vfs.New(t.Context(), backend, &opt)
	t.Cleanup(V.Shutdown)
	h := &Handler{vfs: V, billyFS: &FS{vfs: V}, opt: Options{HandleCache: cacheStable}}
	h.Cache, err = h.getCache()
	require.NoError(t, err)
	root, err := h.vfs.Root()
	require.NoError(t, err)
	root.ForgetAll()
	first, err := h.billyFS.Stat("existing.txt")
	require.NoError(t, err)
	firstID := first.Sys().(*nfsfile.FileInfo).Fileid
	handle := h.ToHandle(h.billyFS, []string{"existing.txt"})
	root.ForgetAll()
	second, err := h.billyFS.Stat("existing.txt")
	require.NoError(t, err)
	require.Equal(t, firstID, second.Sys().(*nfsfile.FileInfo).Fileid)
	require.Equal(t, handle, h.ToHandle(h.billyFS, []string{"existing.txt"}))
}

func TestStableCacheConcurrent(t *testing.T) {
	h := newStableTestHandler(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p := []string{fmt.Sprintf("entry-%d", j)}
				a := h.ToHandle(h.billyFS, p)
				b := h.ToHandle(h.billyFS, p)
				if string(a) != string(b) {
					t.Error("unstable identity")
				}
				_, _, err := h.FromHandle(a)
				if err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func resolvedTestPath(t *testing.T, h *Handler, p []string) string {
	t.Helper()
	result, err := h.billyFS.resolvePath(strings.Join(p, "/"))
	require.NoError(t, err)
	return result
}

func TestStableHandleResolvedAfterNameReuse(t *testing.T) {
	h := newStableTestHandler(t)
	handle := h.ToHandle(h.billyFS, []string{"sub", "hello.txt"})
	requestFS, requestPath, err := h.FromHandle(handle)
	require.NoError(t, err)
	require.NoError(t, h.billyFS.Rename("sub/hello.txt", "sub/moved.txt"))
	replacement, err := h.billyFS.Create("sub/hello.txt")
	require.NoError(t, err)
	_, err = replacement.Write([]byte("unrelated"))
	require.NoError(t, err)
	require.NoError(t, replacement.Close())
	old, err := requestFS.Open(requestFS.Join(requestPath...))
	require.NoError(t, err)
	data, err := io.ReadAll(old)
	require.NoError(t, err)
	require.NoError(t, old.Close())
	require.Equal(t, "world", string(data), "request must retain identity across name reuse")
}
