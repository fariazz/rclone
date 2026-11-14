//go:build unix

package nfs

import (
	"io"
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	gonfs "github.com/willscott/go-nfs"
	clientnfs "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

// Exercise real RPC encoding/dispatch without requiring a privileged mount.
func TestStableProtocolOpenRename(t *testing.T) {
	h := newStableTestHandler(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- gonfs.Serve(listener, h) }()
	t.Cleanup(func() { require.NoError(t, listener.Close()); <-done })
	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	portNumber, err := strconv.Atoi(port)
	require.NoError(t, err)
	client, err := clientnfs.DialServiceAtPort(host, portNumber)
	require.NoError(t, err)
	t.Cleanup(client.Close)
	mount := &clientnfs.Mount{Client: client}
	target, err := mount.Mount("/", rpc.AuthNull)
	require.NoError(t, err)
	file, err := target.Open("sub/hello.txt")
	require.NoError(t, err)
	require.NoError(t, target.Rename("sub/hello.txt", "sub/renamed.txt"))
	require.NoError(t, target.Rename("sub", "moved"))
	data := make([]byte, 5)
	n, err := file.ReadAt(data, 0)
	require.True(t, err == nil || err == io.EOF, "%v", err)
	require.Equal(t, 5, n)
	require.Equal(t, "world", string(data))
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(t, err)
	n, err = file.Write([]byte("saved"))
	require.NoError(t, err)
	require.Equal(t, 5, n)
	require.NoError(t, file.Close())
	current, err := target.Open("moved/renamed.txt")
	require.NoError(t, err)
	n, err = current.ReadAt(data, 0)
	require.True(t, err == nil || err == io.EOF, "%v", err)
	require.Equal(t, 5, n)
	require.Equal(t, "saved", string(data))
	require.NoError(t, current.Close())
}
