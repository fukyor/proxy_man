package myminio

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withTempWorkingDir(t *testing.T) {
	t.Helper()

	oldDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(oldDir))
	})
}

func newMockMinioClient(t *testing.T) *Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if _, ok := req.URL.Query()["location"]; ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`))
			return
		}

		if req.Body != nil {
			if _, err := io.Copy(io.Discard, req.Body); err != nil {
				_ = req.Body.Close()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = req.Body.Close()
		}

		w.Header().Set("ETag", `"mock-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{
		Creds:  credentials.NewStaticV4("mock-key", "mock-secret", ""),
		Secure: false,
	})
	require.NoError(t, err)

	return &Client{
		Client: client,
		Config: Config{
			Endpoint: "mock.local:9000",
			Bucket:   "test-bucket",
			Enabled:  true,
		},
	}
}

func TestUploadViaTempFileCleansTempFile(t *testing.T) {
	withTempWorkingDir(t)

	client := newMockMinioClient(t)
	data := []byte(`{"message":"chunked body"}`)
	reader := client.BuildBodyReader(io.NopCloser(bytes.NewReader(data)), 10086, "resp", "application/json", -1)

	copied, err := io.Copy(io.Discard, reader)
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), copied)
	require.NoError(t, reader.Close())
	require.NoError(t, reader.Capture.Error)
	assert.True(t, reader.Capture.Uploaded)

	files, err := filepath.Glob(filepath.Join("myminio", "tmp", "upload-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, files)
}
