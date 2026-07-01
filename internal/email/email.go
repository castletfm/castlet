// Package email canonicalizes user email addresses so Castlet's identity model
// is "one mailbox, one account".
package email

import (
	"net/mail"
	"strings"
)

// Canonical parses raw as a single email address, rejecting malformed input,
// and returns a canonical form suitable for both storage and identity
// comparison.
//
// It strips any display name ("Bob <bob@x>" -> "bob@x") and lower-cases the
// WHOLE address, not just the domain. RFC 5321 permits case-sensitive local
// parts, but in practice every mailbox provider treats them case-insensitively,
// and Castlet keys accounts on the mailbox. Lower-casing the whole address is
// the pragmatic choice for this model: it guarantees Alice@Example.com and
// alice@example.com resolve to the same account instead of silently becoming
// two, and lets OIDC link a pre-existing password account by email regardless
// of the case the provider reports. Callers that need to preserve the address
// exactly as typed (e.g. to echo it back on a form) should keep the raw value
// separately; the return value here is the identity key.
func Canonical(raw string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return strings.ToLower(addr.Address), nil
}
