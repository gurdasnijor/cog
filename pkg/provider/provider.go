package provider

import (
	"context"

	"github.com/replicate/cog/pkg/config"
)

// PushOptions contains all options for a push operation
type PushOptions struct {
	Image      string
	Config     *config.Config
	ProjectDir string
}

type LoginOptions struct {
	TokenStdin bool
	Host       string
}

// Provider encapsulates registry-specific behavior
type Provider interface {
	// Name returns the provider identifier (e.g., "replicate", "generic")
	Name() string

	// MatchesRegistry returns true if this provider handles the given registry host
	MatchesRegistry(host string) bool

	// Login performs provider-specific authentication
	Login(ctx context.Context, opts LoginOptions) error

	// PostPush is called after push attempt (success or failure)
	// - Shows success message (e.g., Replicate model URL)
	// - May transform errors into provider-specific messages
	// - pushErr is nil on success, contains the push error on failure
	PostPush(ctx context.Context, opts PushOptions, pushErr error) error
}

// Publisher is an optional provider capability for publishing model source
// directly. Providers that implement Publisher own the complete publication
// flow, so the CLI does not initialize Docker or push an OCI image.
type Publisher interface {
	Publish(ctx context.Context, opts PushOptions) error
}
