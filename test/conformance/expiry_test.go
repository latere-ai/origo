// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package conformance

import (
	"encoding/base64"
	"testing"
	"time"
)

// expiryOf reads a token's exp, and a token whose payload does not decode
// is an error rather than an expiry at the epoch.
func TestExpiryOf(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"dev","exp":1790000000}`))
	got, err := expiryOf("e30." + payload + ".sig")
	if err != nil || !got.Equal(time.Unix(1790000000, 0)) {
		t.Fatalf("expiryOf = %v, %v", got, err)
	}
	if _, err := expiryOf("not a token"); err == nil {
		t.Fatal("a value that is no token has an expiry")
	}
}
