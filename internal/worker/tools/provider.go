package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Source says where a resolved binary came from.
type Source string

// The provider sources, in default resolution order.
const (
	SourceConfigured Source = "configured"
	SourceBundled    Source = "bundled"
	SourceManaged    Source = "managed"
	SourcePath       Source = "path"
)

// Resolved is a binary that passed verification.
type Resolved struct {
	// Tool is the tool name, e.g. "rclone".
	Tool string
	// Path is the absolute path of the executable.
	Path string
	// Version is the version it reports, equal to the pin.
	Version string
	// Source is the provider that produced it.
	Source Source
	// Rejected lists candidates from earlier providers that were found but
	// failed verification (not mere absence). Callers should log them.
	Rejected []Rejection
}

// Provider finds a verified binary for a pinned tool.
type Provider interface {
	// Source identifies the provider.
	Source() Source
	// Resolve returns a binary that is verified to be spec.Version. It
	// returns an error wrapping ErrNotFound when the provider has no
	// candidate at all, and any other error when a candidate exists but is
	// unusable.
	Resolve(ctx context.Context, spec Spec) (Resolved, error)
}

// Resolver tries providers in order.
type Resolver struct {
	manifest  Manifest
	providers []Provider
}

// NewResolver returns a Resolver that tries providers in the order given.
func NewResolver(manifest Manifest, providers ...Provider) (*Resolver, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &Resolver{manifest: manifest, providers: slices.Clone(providers)}, nil
}

// Manifest returns the manifest the resolver verifies against.
func (r *Resolver) Manifest() Manifest { return r.manifest }

// Resolve returns the first verified binary for tool. If no provider has one
// the error is a *ResolveError listing why each candidate was rejected.
func (r *Resolver) Resolve(ctx context.Context, tool string) (Resolved, error) {
	spec, ok := r.manifest.Lookup(tool)
	if !ok {
		return Resolved{}, fmt.Errorf("%w: %q", ErrUnknownTool, tool)
	}
	var rejected, all []Rejection
	for _, p := range r.providers {
		if err := ctx.Err(); err != nil {
			return Resolved{}, fmt.Errorf("tools: resolve %s: %w", tool, err)
		}
		res, err := p.Resolve(ctx, spec)
		if err == nil {
			res.Tool = spec.Name
			res.Source = p.Source()
			res.Rejected = rejected
			return res, nil
		}
		if isContextErr(err) && ctx.Err() != nil {
			return Resolved{}, fmt.Errorf("tools: resolve %s: %w", tool, err)
		}
		rej := Rejection{Source: p.Source(), Err: err}
		all = append(all, rej)
		if !errors.Is(err, ErrNotFound) {
			// Mere absence is only worth reporting when nothing else works.
			rejected = append(rejected, rej)
		}
	}
	return Resolved{}, &ResolveError{Tool: tool, Rejected: all}
}

// Attempt is the outcome of one provider in Trace.
type Attempt struct {
	Source   Source
	Resolved Resolved // valid when Err is nil
	Err      error
}

// Trace asks every provider about tool, without stopping at the first
// success. It is meant for diagnostics such as `tools status`.
func (r *Resolver) Trace(ctx context.Context, tool string) ([]Attempt, error) {
	spec, ok := r.manifest.Lookup(tool)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTool, tool)
	}
	attempts := make([]Attempt, 0, len(r.providers))
	for _, p := range r.providers {
		if err := ctx.Err(); err != nil {
			return attempts, fmt.Errorf("tools: trace %s: %w", tool, err)
		}
		res, err := p.Resolve(ctx, spec)
		res.Tool, res.Source = spec.Name, p.Source()
		attempts = append(attempts, Attempt{Source: p.Source(), Resolved: res, Err: err})
	}
	return attempts, nil
}
