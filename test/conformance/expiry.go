// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"time"

	"latere.ai/x/pkg/authkit/jwt"
)

// expiryOf is the exp a token carries, read without verifying it: the
// suite holds an installation to what it answers about a token the run
// presented, and the installation has already verified that token.
func expiryOf(token string) (time.Time, error) {
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := jwt.DecodePayload(token, &claims); err != nil {
		return time.Time{}, err
	}
	return time.Unix(int64(claims.Exp), 0), nil
}
