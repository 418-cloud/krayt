package sandbox

import "testing"

func TestSecretPlaceholder(t *testing.T) {
	if got := SecretPlaceholder("ANTHROPIC_API_KEY"); got != "$MSB_ANTHROPIC_API_KEY" {
		t.Errorf("SecretPlaceholder(ANTHROPIC_API_KEY) = %q, want $MSB_ANTHROPIC_API_KEY", got)
	}
}
