package zinnia

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/go-archive"
	"github.com/moby/patternmatcher/ignorefile"
	"golang.org/x/term"

	"github.com/replicate/cog/pkg/docker"
	"github.com/replicate/cog/pkg/global"
	"github.com/replicate/cog/pkg/provider"
	"github.com/replicate/cog/pkg/schema/openapi"
	"github.com/replicate/cog/pkg/util/console"
)

const defaultHost = "replicate.zinnia.page"

type ZinniaProvider struct {
	host      string
	baseURL   string
	client    *http.Client
	loadToken func(context.Context, string) (string, error)
}

func New() *ZinniaProvider {
	host := os.Getenv("COG_ZINNIA_HOST")
	if host == "" {
		host = defaultHost
	}
	return &ZinniaProvider{
		host:      host,
		baseURL:   addressWithScheme(host),
		client:    http.DefaultClient,
		loadToken: docker.LoadLoginToken,
	}
}

func (p *ZinniaProvider) Name() string {
	return "zinnia"
}

func (p *ZinniaProvider) MatchesRegistry(host string) bool {
	return host == p.host
}

func (p *ZinniaProvider) Login(ctx context.Context, opts provider.LoginOptions) error {
	var token string
	if opts.TokenStdin {
		value, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read token: %w", err)
		}
		token = string(value)
	} else {
		fmt.Printf("Publisher token for %s: ", opts.Host)
		value, err := term.ReadPassword(int(os.Stdin.Fd())) //nolint:gosec // Fd fits in int on supported platforms
		fmt.Println()
		if err != nil {
			return fmt.Errorf("failed to read token: %w", err)
		}
		token = string(value)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("token cannot be empty")
	}
	if err := docker.SaveLoginToken(ctx, opts.Host, "token", token); err != nil {
		return err
	}
	console.Successf("Login succeeded for %s", console.Bold(opts.Host))
	return nil
}

func (p *ZinniaProvider) PostPush(context.Context, provider.PushOptions, error) error {
	return nil
}

func (p *ZinniaProvider) Publish(ctx context.Context, opts provider.PushOptions) error {
	owner, name, err := modelName(opts.Image, p.host)
	if err != nil {
		return err
	}
	schema, err := openapi.GenerateSchema(opts.Config, opts.ProjectDir)
	if err != nil {
		return fmt.Errorf("generate OpenAPI schema: %w", err)
	}
	source, err := sourceArchive(opts.ProjectDir)
	if err != nil {
		return fmt.Errorf("archive model source: %w", err)
	}
	defer source.Close()
	token, err := p.loadToken(ctx, p.host)
	if err != nil {
		return fmt.Errorf("load credentials for %s: %w", p.host, err)
	}

	reader, contentType := publicationBody(schema, source)
	endpoint := fmt.Sprintf(
		"%s/cog/v1/models/%s/%s/versions",
		strings.TrimRight(p.baseURL, "/"),
		url.PathEscape(owner),
		url.PathEscape(name),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	// The destination is the explicitly selected provider host, not model input.
	resp, err := p.client.Do(req) //nolint:gosec // provider base URL is trusted configuration
	if err != nil {
		return fmt.Errorf("publish model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("publish model: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var result struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		URL   string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode publication response: %w", err)
	}
	console.Successf("Published %s@%s", result.Model, result.ID)
	if result.URL != "" {
		console.Infof("Deploy this model at:\n    %s", console.Bold(result.URL))
	}
	return nil
}

func publicationBody(schema []byte, source io.Reader) (io.ReadCloser, string) {
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	go func() {
		err := writePublication(multipartWriter, schema, source)
		if closeErr := multipartWriter.Close(); err == nil {
			err = closeErr
		}
		_ = writer.CloseWithError(err)
	}()
	return reader, multipartWriter.FormDataContentType()
}

func writePublication(writer *multipart.Writer, schema []byte, source io.Reader) error {
	if err := writer.WriteField("cog_version", global.Version); err != nil {
		return err
	}
	schemaPart, err := writer.CreateFormField("openapi_schema")
	if err != nil {
		return err
	}
	if _, err := schemaPart.Write(schema); err != nil {
		return err
	}
	sourcePart, err := writer.CreateFormFile("source", "source.tar")
	if err != nil {
		return err
	}
	_, err = io.Copy(sourcePart, source)
	return err
}

func sourceArchive(projectDir string) (io.ReadCloser, error) {
	excludes := []string{".git", ".cog"}
	ignore, err := os.Open(filepath.Join(projectDir, ".dockerignore"))
	if err == nil {
		defer ignore.Close()
		patterns, readErr := ignorefile.ReadAll(ignore)
		if readErr != nil {
			return nil, readErr
		}
		excludes = append(excludes, patterns...)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return archive.TarWithOptions(projectDir, &archive.TarOptions{
		ExcludePatterns: excludes,
	})
}

func modelName(image, host string) (string, string, error) {
	value := strings.TrimPrefix(image, host+"/")
	if at := strings.Index(value, "@"); at >= 0 {
		value = value[:at]
	}
	if slash := strings.LastIndex(value, "/"); slash >= 0 {
		if colon := strings.Index(value[slash:], ":"); colon >= 0 {
			value = value[:slash+colon]
		}
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("Zinnia model must use %s/<owner>/<name>", host)
	}
	return parts[0], parts[1], nil
}

func addressWithScheme(host string) string {
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		return "http://" + host
	}
	return "https://" + host
}

var (
	_ provider.Provider  = (*ZinniaProvider)(nil)
	_ provider.Publisher = (*ZinniaProvider)(nil)
)
