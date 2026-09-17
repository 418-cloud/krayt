package sandbox

// SecretPlaceholder returns msb's own default placeholder string for a secret key — the value
// the guest sees under that key's env var once msb has been told to substitute it (§6.14, §8.2's
// "There is no /run/secrets under msb"). krayt never overrides msb's default with a `--secret
// NAME@HOST=PLACEHOLDER` form, so this is exactly what a guest process observes.
//
// Pure and pinned by a unit test: verified on hardware by P5 (§14 Phase 11,
// $MSB_ANTHROPIC_API_KEY), again inside a `krayt shell` session (run_5e8392ed,
// $MSB_CLAUDE_CODE_OAUTH_TOKEN, §14 Phase 12), and against msb 0.6.16's
// crates/network/lib/secrets/config.rs.
func SecretPlaceholder(key string) string {
	return "$MSB_" + key
}
