package golang

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khulnasoft/inngest/pkg/coreapi/graph/models"
	"github.com/khulnasoft/inngest/pkg/inngest"
	"github.com/khulnasoft/inngest/tests/client"
	"github.com/khulnasoft-lab/inngestgo"
	"github.com/khulnasoft-lab/inngestgo/step"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testCancelEvt inngestgo.GenericEvent[any, any]

func TestEventCancellation(t *testing.T) {
	ctx := context.Background()

	c := client.New(t)
	appName := uuid.New().String()
	h, server, registerFuncs := NewSDKHandler(t, appName)
	defer server.Close()

	var (
		runCounter   int32
		runCancelled int32
		runID        string
	)

	triggerEvtName := uuid.New().String()
	cancelEvtName := uuid.New().String()

	a := inngestgo.CreateFunction(
		inngestgo.FunctionOpts{
			Name: "test-cancel",
			Cancel: []inngest.Cancel{
				{Event: cancelEvtName, If: inngestgo.StrPtr("async.data.cancel == event.data.cancel")},
			},
		},
		inngestgo.EventTrigger(triggerEvtName, nil),
		func(ctx context.Context, input inngestgo.Input[testCancelEvt]) (any, error) {
			_, _ = step.Run(ctx, "do something", func(ctx context.Context) (any, error) {
				runID = input.InputCtx.RunID
				fmt.Println("HELLO")

				atomic.AddInt32(&runCounter, 1)
				return nil, nil
			})

			step.Sleep(ctx, "stop", 30*time.Second)

			_, _ = step.Run(ctx, "should not happen", func(ctx context.Context) (any, error) {
				atomic.AddInt32(&runCounter, 1)
				return nil, nil
			})

			return true, nil
		},
	)

	cf := inngestgo.CreateFunction(
		inngestgo.FunctionOpts{Name: "handle-cancel"},
		inngestgo.EventTrigger(
			"inngest/function.cancelled",
			inngestgo.StrPtr(fmt.Sprintf(
				"event.data.function_id == '%s-test-cancel'",
				appName,
			)),
		),
		func(ctx context.Context, input inngestgo.Input[any]) (any, error) {
			fmt.Println("CANCELLED")

			atomic.AddInt32(&runCancelled, 1)

			return true, nil
		},
	)

	h.Register(a, cf)
	registerFuncs()

	evt := inngestgo.Event{
		Name: triggerEvtName,
		Data: map[string]any{"cancel": 1},
	}
	_, err := inngestgo.Send(ctx, evt)
	require.NoError(t, err)

	<-time.After(3 * time.Second)

	t.Run("check run", func(t *testing.T) {
		require.Equal(t, int32(1), atomic.LoadInt32(&runCounter))
		require.Equal(t, int32(0), atomic.LoadInt32(&runCancelled))
	})

	t.Run("should cancel run", func(t *testing.T) {
		r := require.New(t)
		_, err := inngestgo.Send(ctx, inngestgo.Event{
			Name: cancelEvtName,
			Data: map[string]any{"cancel": 1},
		})
		r.NoError(err)

		r.EventuallyWithT(func(t *assert.CollectT) {
			a := assert.New(t)
			a.Equal(int32(1), atomic.LoadInt32(&runCounter))
			a.Equal(int32(1), atomic.LoadInt32(&runCancelled))
		}, 10*time.Second, 1*time.Second)
	})

	t.Run("trace run should have appropriate data", func(t *testing.T) {
		run := c.WaitForRunTraces(ctx, t, &runID, client.WaitForRunTracesOptions{
			Status:         models.FunctionStatusCancelled,
			Timeout:        10 * time.Second,
			Interval:       500 * time.Millisecond,
			ChildSpanCount: 2,
		})

		require.Equal(t, models.RunTraceSpanStatusCancelled.String(), run.Trace.Status)
	})
}
