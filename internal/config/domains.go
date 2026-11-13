package config

import "slices"

// DefaultAllowDomains are always allowed in allowlist mode (spec 3.1): the
// agents' API and login endpoints, nothing else. Keep this list short and
// auditable.
//
// Deliberately excluded: telemetry and error reporting (statsig, sentry.io,
// datadog, ...). Shared third-party services where anyone can create a
// project are open exfiltration channels; sessions set DISABLE_TELEMETRY and
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC so agents do not wait on them.
var DefaultAllowDomains = []string{
	// Claude Code: API, and OAuth login.
	"api.anthropic.com",
	"claude.ai",
	"console.anthropic.com",
	// Codex: API, and ChatGPT login and backend.
	"api.openai.com",
	"auth.openai.com",
	"chatgpt.com",
	// opencode: its own login/service and the model catalog it loads.
	"opencode.ai",
	"models.dev",
}

// AllowDomainsWithDefaults returns the defaults plus the configured domains.
func (c Config) AllowDomainsWithDefaults() []string {
	out := slices.Clone(DefaultAllowDomains)
	for _, d := range c.AllowDomains {
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}
