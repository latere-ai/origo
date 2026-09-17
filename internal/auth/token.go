// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/json"
	"slices"

	"latere.ai/x/pkg/authkit/jwt"
)

// MaxTokenBytes is the size above which a token is refused before it is
// parsed. It is the figure spec 007 names and the shared verifier's own
// bound, handed to it as jwt.Config.MaxTokenBytes.
const MaxTokenBytes = 8 << 10

// The reasons of spec 003's unauthenticated table, one per row of spec
// 007's verification table. Ten of them are the shared verifier's words
// for the same rows, written here as its constants so the two tables
// cannot drift; the other five are Origo's own, for rows the shared
// package does not carry.
const (
	ReasonMissing           = "missing"
	ReasonSize              = string(jwt.ReasonTooLarge)
	ReasonMalformed         = string(jwt.ReasonMalformed)
	ReasonSignature         = string(jwt.ReasonBadSignature)
	ReasonIssuer            = string(jwt.ReasonBadIssuer)
	ReasonIssuerUnavailable = string(jwt.ReasonIssuerUnavailable)
	ReasonUnknownKey        = string(jwt.ReasonUnknownKey)
	ReasonAudience          = string(jwt.ReasonBadAudience)
	ReasonExpired           = string(jwt.ReasonExpired)
	ReasonNBF               = string(jwt.ReasonNotYetValid)
	ReasonIAT               = string(jwt.ReasonTooOld)
	ReasonSubject           = "subject"
	// ReasonDelegation refuses a token carrying an act claim: no token
	// carries a chain (the family's decision D5), and a service acting
	// for a person presents the token its issuer minted for that person.
	ReasonDelegation = "delegation"
)

// Refusal is why a credential was refused: one reason of the table,
// sent as details.reason of a 401.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return "auth: token refused: " + r.Reason }

func refuse(reason string) error { return &Refusal{Reason: reason} }

// Audience decodes the aud claim in both forms a JWT allows: one string
// or a list of strings.
type Audience []string

// UnmarshalJSON accepts both forms.
func (a *Audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// Claims are the claims Origo reads for itself, decoded through
// jwt.DecodePayload and never through a parser of its own: the issuer
// that paces the fetch, the three rows of spec 007's table the shared
// verifier does not carry (`aud` in the table's place, `sub`, and the
// `act` the raw claims below name), the `exp` the verified-token cache
// is bounded by, and the two fields a repository-bound token binds.
// Every other row is the shared verifier's. Times are Unix seconds, and
// an absent exp decodes to zero, which is the epoch and so expired.
type Claims struct {
	Iss   string   `json:"iss"`
	Sub   string   `json:"sub"`
	Aud   Audience `json:"aud"`
	Exp   float64  `json:"exp"`
	Repo  string   `json:"repo"`
	Scope string   `json:"scope"`
}

// HasAudience reports whether aud contains the value.
func (c Claims) HasAudience(want string) bool { return slices.Contains(c.Aud, want) }
