package router

import "shunt/internal/config"

// Sanitize is the per-provider cleanup profile. Only knobs that really differ
// between Anthropic-compatible providers live here; rules that apply to every
// third party (billing block, credentials, server tools) are not configurable.
type Sanitize struct {
	BetaAllow  []string // anthropic-beta tokens forwarded; empty = drop the header
	TopKeys    []string // body top-level allowlist; everything else is dropped
	BlockTypes []string // allowed message content block types
}

// profile is the sanitize profile of a provider, defaults filled in.
func profile(p config.Provider) Sanitize {
	p = p.WithDefaults()
	return Sanitize{BetaAllow: p.BetaAllow, TopKeys: p.TopKeys, BlockTypes: p.BlockTypes}
}
