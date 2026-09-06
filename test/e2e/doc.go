// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

// Package e2e is the end-to-end suite: a built origod process against
// MinIO with the real git client. Its tests carry the e2e build tag and
// run through `make test-integration`; this file gives the package a
// compilation unit in every build so the tooling that lints the
// packages a push touches can type-check it.
package e2e
