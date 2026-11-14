//go:build unix

package nfs

import (
	"errors"
	"testing"

	"github.com/rclone/rclone/vfs"
	"github.com/stretchr/testify/require"
)

type syncTestHandle struct {
	vfs.Handle
	calls             []string
	syncErr, closeErr error
}

func (h *syncTestHandle) Sync() error  { h.calls = append(h.calls, "sync"); return h.syncErr }
func (h *syncTestHandle) Close() error { h.calls = append(h.calls, "close"); return h.closeErr }

func TestNFSWriteCloseSync(t *testing.T) {
	for _, failSync := range []bool{false, true} {
		h := &syncTestHandle{}
		if failSync {
			h.syncErr = errors.New("disk sync failure")
		}
		f := &syncFile{Handle: h}
		err := f.Close()
		require.Equal(t, []string{"sync", "close"}, h.calls)
		if failSync {
			require.ErrorIs(t, err, h.syncErr)
		} else {
			require.NoError(t, err)
		}
	}
	h := &syncTestHandle{closeErr: errors.New("close failure")}
	require.ErrorIs(t, (&syncFile{Handle: h}).Close(), h.closeErr)
}
