package redis_state

import (
	"context"

	osqueue "github.com/khulnasoft/inngest/pkg/execution/queue"
)

// QueueItemIndex represends a set of indexes for a given queue item.  We currently allow
// up to 2 indexes per job item to be created.
//
// # What is an index?
//
// An index is a sorted ZSET of job items for a given key.  The ZSET stores all
// oustanding AND in-progress job IDs, scored by job time in milliseconds. Because this
// stores outstanding and in progress jobs, this _cannot_ be used to control concurrency.
// It is used to specifically list all jobs that exist for given keys for transparency.
//
// A nil slice or empty strings within the slice indicate nil indexes, ie. an index
// will not be created.
type QueueItemIndex [2]string

// QueueItemIndexer represents a function which generates indexes for a given queue item.
type QueueItemIndexer func(ctx context.Context, i osqueue.QueueItem, kg QueueKeyGenerator) QueueItemIndex

// QueueItemIndexerFunc returns default queue item indexes for a given queue item.
//
// Reasonably, these indexes should always be provided for queue implementation.  If a
// QueueItemIndexer is not provided, this function will be used with an "{queue}" predix.
func QueueItemIndexerFunc(ctx context.Context, i osqueue.QueueItem, kg QueueKeyGenerator) QueueItemIndex {
	switch i.Data.Kind {
	case osqueue.KindStart:
		return QueueItemIndex{
			kg.RunIndex(i.Data.Identifier.RunID),
			kg.Status("start", i.FunctionID),
		}
	case osqueue.KindEdge, osqueue.KindEdgeError:
		// For edges and sleeps, store an index for the given run ID.
		return QueueItemIndex{
			kg.RunIndex(i.Data.Identifier.RunID),
			kg.Status("in-progress", i.FunctionID),
		}
	case osqueue.KindSleep:
		return QueueItemIndex{
			kg.RunIndex(i.Data.Identifier.RunID),
			kg.Status("sleep", i.FunctionID),
		}
	case osqueue.KindPause:
		// Still keep this in the run index so that we know jobs are present
		// for the run.
		return QueueItemIndex{
			kg.RunIndex(i.Data.Identifier.RunID),
		}
	}
	return QueueItemIndex{}
}
