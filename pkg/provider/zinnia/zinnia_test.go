package zinnia

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/replicate/cog/pkg/config"
	"github.com/replicate/cog/pkg/provider"
)

func TestMatchesRegistry(t *testing.T) {
	p := &ZinniaProvider{host: defaultHost}

	assert.True(t, p.MatchesRegistry(defaultHost))
	assert.False(t, p.MatchesRegistry("r8.im"))
}

func TestModelName(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		wantOwner string
		wantName  string
		wantErr   string
	}{
		{
			name:      "plain reference",
			image:     defaultHost + "/bodyiq/minimax-h3",
			wantOwner: "bodyiq",
			wantName:  "minimax-h3",
		},
		{
			name:      "tagged reference",
			image:     defaultHost + "/bodyiq/minimax-h3:latest",
			wantOwner: "bodyiq",
			wantName:  "minimax-h3",
		},
		{
			name:    "missing owner",
			image:   defaultHost + "/minimax-h3",
			wantErr: "Zinnia model must use",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, name, err := modelName(tt.image, defaultHost)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantName, name)
		})
	}
}

func TestPublish(t *testing.T) {
	projectDir := t.TempDir()
	writeFile(t, filepath.Join(projectDir, "cog.yaml"), "predict: predict.py:Predictor\n")
	writeFile(t, filepath.Join(projectDir, "predict.py"), `from cog import BasePredictor

class Predictor(BasePredictor):
    def predict(self, prompt: str) -> str:
        return prompt
`)
	writeFile(t, filepath.Join(projectDir, ".dockerignore"), "ignored.txt\n")
	writeFile(t, filepath.Join(projectDir, "ignored.txt"), "not published")
	writeFile(t, filepath.Join(projectDir, ".git", "config"), "not published")
	writeFile(t, filepath.Join(projectDir, ".cog", "state"), "not published")

	var received bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/cog/v1/models/bodyiq/minimax-h3/versions", r.URL.Path)
		assert.Equal(t, "Bearer publisher-token", r.Header.Get("Authorization"))
		require.NoError(t, r.ParseMultipartForm(4<<20))
		assert.NotEmpty(t, r.FormValue("cog_version"))

		var schema map[string]any
		require.NoError(t, json.Unmarshal([]byte(r.FormValue("openapi_schema")), &schema))
		assert.Contains(t, schema, "components")

		file, _, err := r.FormFile("source")
		require.NoError(t, err)
		defer file.Close()
		entries := tarEntries(t, file)
		assert.Contains(t, entries, "cog.yaml")
		assert.Contains(t, entries, "predict.py")
		assert.NotContains(t, entries, "ignored.txt")
		assert.NotContains(t, entries, ".git/config")
		assert.NotContains(t, entries, ".cog/state")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, err = io.WriteString(w, `{"id":"sha256:abc","model":"bodyiq/minimax-h3","url":"https://replicate.zinnia.page/bodyiq/minimax-h3"}`)
		require.NoError(t, err)
	}))
	defer server.Close()

	p := &ZinniaProvider{
		host:    defaultHost,
		baseURL: server.URL,
		client:  server.Client(),
		loadToken: func(context.Context, string) (string, error) {
			return "publisher-token", nil
		},
	}
	err := p.Publish(context.Background(), provider.PushOptions{
		Image:      defaultHost + "/bodyiq/minimax-h3",
		Config:     &config.Config{Predict: "predict.py:Predictor"},
		ProjectDir: projectDir,
	})
	require.NoError(t, err)
	assert.True(t, received)
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
}

func tarEntries(t *testing.T, reader io.Reader) []string {
	t.Helper()
	var entries []string
	tarr := tar.NewReader(reader)
	for {
		header, err := tarr.Next()
		if err == io.EOF {
			return entries
		}
		require.NoError(t, err)
		entries = append(entries, header.Name)
	}
}
