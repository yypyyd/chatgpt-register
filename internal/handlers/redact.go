package handlers

import "regexp"

var (
	jwtLikePattern    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\b`)
	jsonSecretPattern = regexp.MustCompile(`(?i)("(?:access_?token|refresh_?token|password|cookie)"\s*:\s*")[^"]*(")`)
)

// redactCredentialText is a final defense for legacy registration logs. New
// credential code never logs secret values, but historical rows may contain a
// truncated session response from older builds.
func redactCredentialText(text string) string {
	text = jwtLikePattern.ReplaceAllString(text, "[REDACTED_TOKEN]")
	return jsonSecretPattern.ReplaceAllString(text, `${1}[REDACTED]${2}`)
}
