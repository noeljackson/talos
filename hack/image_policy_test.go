// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package hack_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildEmbedsGeneratedSELinuxPolicy(t *testing.T) {
	contents, err := os.ReadFile("../Dockerfile")
	require.NoError(t, err)
	_, base, ok := strings.Cut(string(contents), "FROM build-go AS base\n")
	require.True(t, ok)
	base, _, ok = strings.Cut(base, "\nFROM ")
	require.True(t, ok)

	// COPY destinations are not relative to WORKDIR when they start with '/'.
	// The embed inputs live under /src, not the container's /internal directory.
	generatedCopy := "COPY --link --from=selinux-generate / /src/internal/pkg/selinux/"
	require.Contains(t, base, generatedCopy)
	assert.Greater(t, strings.Index(base, generatedCopy), strings.Index(base, "COPY ./internal ./internal"))
	assert.Less(t, strings.Index(base, generatedCopy), strings.Index(base, "go list all"))
	assert.NotContains(t, base, "COPY --link --from=selinux-generate / /internal/")
}
