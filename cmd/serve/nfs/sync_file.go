//go:build unix

package nfs

import (
	"errors"
	"github.com/rclone/rclone/vfs"
)

// syncFile calls the VFS Sync operation before go-nfs acknowledges a WRITE
// as FILE_SYNC. With VFS caching this flushes to the local cache; remote
// upload still follows the configured write-back delay.
type syncFile struct{ vfs.Handle }

// Close syncs cached data and closes the handle even if syncing fails.
func (f *syncFile) Close() error {
	syncErr := f.Handle.Sync()
	closeErr := f.Handle.Close()
	return errors.Join(syncErr, closeErr)
}
