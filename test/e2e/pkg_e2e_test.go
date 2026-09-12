// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"testing"

	"latere.ai/x/pkg/s3/s3test"
)

// TestE2ESharedPrimitivesPushAndClone runs the real node and Git clients against
// the signed in-process object store, so shared-library migrations are verified
// without an external bucket. It includes a restart onto an empty disk.
func TestE2ESharedPrimitivesPushAndClone(t *testing.T) {
	store := s3test.New(t, "origo")
	t.Setenv("ORIGO_TEST_S3_ENDPOINT", store.URL())
	t.Setenv("ORIGO_TEST_S3_REGION", s3test.Region)
	t.Setenv("ORIGO_TEST_S3_BUCKET", "origo")
	t.Setenv("ORIGO_TEST_S3_KEY", s3test.Key)
	t.Setenv("ORIGO_TEST_S3_SECRET", s3test.Secret)
	t.Setenv("ORIGO_TEST_S3_PATH_STYLE", "1")
	TestE2EPushThenCloneFromAnEmptyDisk(t)
}
