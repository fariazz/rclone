package onedrive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func newContentStreamTest(t *testing.T, handler http.HandlerFunc) *Object {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	transport := redirectTransport{server: server}
	client := &http.Client{Transport: &oauth2.Transport{
		Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "graph_token"}),
		Base:   transport,
	}}
	f := &Fs{
		ci:        fs.GetConfig(context.Background()),
		srv:       rest.NewClient(client).SetRoot("https://graph.microsoft.com/v1.0/drives/test-drive"),
		unAuth:    rest.NewClient(&http.Client{Transport: transport}),
		driveID:   "test-drive",
		driveType: driveTypePersonal,
		pacer:     fs.NewPacer(context.Background(), pacer.NewDefault(pacer.MinSleep(time.Millisecond), pacer.MaxSleep(10*time.Millisecond))),
	}
	f.opt.Region = "global"
	require.NoError(t, configstruct.Set(configmap.Simple{"use_content_stream": "true"}, &f.opt))
	return &Object{fs: f, id: "fake", size: 20}
}

func TestOpenContentStream(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	for _, test := range []struct {
		name    string
		id      string
		path    string
		options []fs.OpenOption
		want    string
	}{
		{"full", "fake", "/beta/drives/test-drive/items/fake/contentStream", nil, string(content)},
		{"range", "fake", "/beta/drives/test-drive/items/fake/contentStream", []fs.OpenOption{&fs.RangeOption{Start: 5, End: 9}}, "56789"},
		{"seek", "fake", "/beta/drives/test-drive/items/fake/contentStream", []fs.OpenOption{&fs.SeekOption{Offset: 15}}, "fghij"},
		{"other drive", "other-drive#fake", "/beta/drives/other-drive/items/fake/contentStream", nil, string(content)},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := newContentStreamTest(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, test.path, r.URL.Path)
				assert.Equal(t, "Bearer graph_token", r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("Prefer"), "ordinary reads must not override malware protection")
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
			})
			o.id = test.id
			in, err := o.Open(context.Background(), test.options...)
			require.NoError(t, err)
			got, err := io.ReadAll(in)
			require.NoError(t, err)
			require.NoError(t, in.Close())
			assert.Equal(t, test.want, string(got))
			assert.Equal(t, int64(len(content)), o.size)
		})
	}
}

func TestOpenContentStreamRangeCheck(t *testing.T) {
	for _, test := range []struct {
		name        string
		code        int
		rangeHeader string
	}{
		{"unexpected range header", http.StatusOK, "bytes 0-4/20"},
		{"wrong range", http.StatusPartialContent, "bytes 0-4/20"},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := newContentStreamTest(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", test.rangeHeader)
				w.WriteHeader(test.code)
				_, _ = io.WriteString(w, "01234")
			})
			in, err := o.Open(context.Background(), &fs.RangeOption{Start: 5, End: 9})
			if in != nil {
				_ = in.Close()
			}
			require.Error(t, err)
			if test.code == http.StatusOK {
				assert.ErrorIs(t, err, fs.ErrorRangeIgnored)
			}
		})
	}
}

func TestOpenContentStreamIgnoredRange(t *testing.T) {
	for _, test := range []struct {
		name   string
		option fs.OpenOption
		want   string
	}{
		{"bounded", &fs.RangeOption{Start: 5, End: 9}, "56789"},
		{"seek", &fs.SeekOption{Offset: 15}, "fghij"},
		{"suffix", &fs.RangeOption{Start: -1, End: 5}, "fghij"},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := newContentStreamTest(t, func(w http.ResponseWriter, r *http.Request) {
				assert.NotEmpty(t, r.Header.Get("Range"))
				_, _ = io.WriteString(w, "0123456789abcdefghij")
			})
			in, err := o.Open(context.Background(), test.option)
			require.NoError(t, err)
			got, err := io.ReadAll(in)
			require.NoError(t, err)
			require.NoError(t, in.Close())
			assert.Equal(t, test.want, string(got))
		})
	}
}

func TestOpenContentStreamError(t *testing.T) {
	o := newContentStreamTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated","message":"Denied"}}`)
	})
	in, err := o.Open(context.Background())
	assert.Nil(t, in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unauthenticated")
}

func TestOpenContentStreamMalwareProtection(t *testing.T) {
	for _, test := range []struct {
		name     string
		override bool
		flagged  bool
		prefer   string
	}{
		{"clean", false, false, ""},
		{"clean with override", true, false, ""},
		{"flagged", false, true, ""},
		{"flagged with override", true, true, "forceInfectedDownload"},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := newContentStreamTest(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, test.prefer, r.Header.Get("Prefer"))
				_, _ = io.WriteString(w, "0123456789abcdefghij")
			})
			o.fs.opt.AVOverride = test.override
			o.meta = &Metadata{malwareDetected: test.flagged}
			in, err := o.Open(context.Background())
			require.NoError(t, err)
			require.NoError(t, in.Close())
		})
	}
}

func TestOpenContentStreamTenantURL(t *testing.T) {
	o := newContentStreamTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("tenant authentication must not be sent to Microsoft Graph")
	})
	o.fs.opt.TenantURL = "https://contoso-my.sharepoint.com/_api"
	in, err := o.Open(context.Background())
	assert.Nil(t, in)
	require.ErrorContains(t, err, "tenant_url must be empty")
}

func TestOpenContentStreamCancellation(t *testing.T) {
	started := make(chan struct{})
	o := newContentStreamTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		in, err := o.Open(ctx)
		if in != nil {
			_ = in.Close()
		}
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		assert.True(t, errors.Is(err, context.Canceled), "%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("download did not stop after cancellation")
	}
}
