package status

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhaseHolder_SetGet(t *testing.T) {
	h := &PhaseHolder{}
	assert.Equal(t, Phase(""), h.Get())

	h.Set(PhaseTask)
	assert.Equal(t, PhaseTask, h.Get())

	h.Set(PhaseReview)
	assert.Equal(t, PhaseReview, h.Get())
}

func TestPhaseHolder_OnChange_Fires(t *testing.T) {
	h := &PhaseHolder{}

	var captured []struct{ old, cur Phase }
	h.OnChange(func(old, cur Phase) {
		captured = append(captured, struct{ old, cur Phase }{old, cur})
	})

	h.Set(PhaseTask)
	h.Set(PhaseReview)

	require.Len(t, captured, 2)
	assert.Equal(t, Phase(""), captured[0].old)
	assert.Equal(t, PhaseTask, captured[0].cur)
	assert.Equal(t, PhaseTask, captured[1].old)
	assert.Equal(t, PhaseReview, captured[1].cur)
}

func TestPhaseHolder_OnChange_NotFiredOnSamePhase(t *testing.T) {
	h := &PhaseHolder{}

	callCount := 0
	h.OnChange(func(_, _ Phase) { callCount++ })

	h.Set(PhaseTask)
	h.Set(PhaseTask) // same phase - should not fire

	assert.Equal(t, 1, callCount)
}

func TestPhaseHolder_OnChange_NilCallbackSafe(t *testing.T) {
	h := &PhaseHolder{}
	// no callback registered - should not panic
	h.Set(PhaseTask)
	assert.Equal(t, PhaseTask, h.Get())
}

func TestPhaseHolder_OnChange_MultipleObservers(t *testing.T) {
	h := &PhaseHolder{}

	var order []string
	h.OnChange(func(_, cur Phase) { order = append(order, "first:"+string(cur)) })
	h.OnChange(func(_, cur Phase) { order = append(order, "second:"+string(cur)) })
	h.OnChange(func(_, cur Phase) { order = append(order, "third:"+string(cur)) })

	h.Set(PhaseTask)
	h.Set(PhaseReview)

	// every observer fires, in registration order, for every change
	assert.Equal(t, []string{
		"first:task", "second:task", "third:task",
		"first:review", "second:review", "third:review",
	}, order)
}

func TestPhaseHolder_OnChange_NilFuncIgnored(t *testing.T) {
	h := &PhaseHolder{}

	callCount := 0
	h.OnChange(nil)
	h.OnChange(func(_, _ Phase) { callCount++ })
	h.OnChange(nil)

	h.Set(PhaseTask) // must not panic on the nil entries

	assert.Equal(t, 1, callCount)
}

func TestPhaseHolder_OnChange_ObserverCanRegisterAnother(t *testing.T) {
	h := &PhaseHolder{}

	// registering from inside a callback must not deadlock: observers run outside the lock
	nested := 0
	h.OnChange(func(_, cur Phase) {
		if cur == PhaseTask {
			h.OnChange(func(_, _ Phase) { nested++ })
		}
	})

	h.Set(PhaseTask) // registers the nested observer, which must not fire for this change
	assert.Equal(t, 0, nested)

	h.Set(PhaseReview) // nested observer is live from here on
	assert.Equal(t, 1, nested)
}

func TestPhaseHolder_ConcurrentOnChange(t *testing.T) {
	h := &PhaseHolder{}

	var cbCount atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	// register observers while phases are being set - exercises the snapshot-under-lock path
	for range 16 {
		wg.Go(func() {
			<-start
			for range 50 {
				h.OnChange(func(_, _ Phase) { cbCount.Add(1) })
			}
		})
	}
	for w := range 16 {
		wg.Go(func() {
			<-start
			for i := range 50 {
				h.Set(Phase(string(rune('a' + (w+i)%7))))
			}
		})
	}

	close(start)
	wg.Wait()

	assert.Positive(t, cbCount.Load())
}

func TestPhaseHolder_ConcurrentAccess(t *testing.T) {
	h := &PhaseHolder{}
	phases := []Phase{
		PhaseTask, PhaseReview, PhaseExternalReview, PhaseExternalEval,
		PhaseCodex, PhaseClaudeEval, PhaseFinalize,
	}

	var cbCount atomic.Int64
	h.OnChange(func(_, _ Phase) {
		_ = h.Get() // exercise read path from callback (deadlock risk if lock held)
		cbCount.Add(1)
	})

	start := make(chan struct{})
	var wg sync.WaitGroup

	workers := 32
	iters := 500
	for w := range workers {
		wg.Go(func() {
			<-start
			for i := range iters {
				h.Set(phases[(w+i)%len(phases)])
				h.Get()
			}
		})
	}

	close(start)
	wg.Wait()

	got := h.Get()
	assert.Contains(t, phases, got)
	assert.Positive(t, cbCount.Load())
}
