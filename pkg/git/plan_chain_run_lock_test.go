package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestService_AcquirePlanChainRunLock(t *testing.T) {
	dir := setupExternalTestRepo(t)
	first, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)
	second, err := NewService(dir, noopServiceLogger())
	require.NoError(t, err)

	releaseFirst, err := first.AcquirePlanChainRunLock("full:one,two")
	require.NoError(t, err)

	_, err = second.AcquirePlanChainRunLock("full:one,two")
	var busyErr *ErrPlanChainBusy
	require.ErrorAs(t, err, &busyErr)
	assert.Equal(t, "full:one,two", busyErr.Identity)

	releaseOther, err := second.AcquirePlanChainRunLock("full:other,chain")
	require.NoError(t, err, "unrelated plan chains must not contend")
	require.NoError(t, releaseOther())

	require.NoError(t, releaseFirst())
	releaseAgain, err := second.AcquirePlanChainRunLock("full:one,two")
	require.NoError(t, err)
	require.NoError(t, releaseAgain())
}
