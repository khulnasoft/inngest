package debounce

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/khulnasoft/inngest/pkg/event"
	"github.com/khulnasoft/inngest/pkg/execution/queue"
	"github.com/khulnasoft/inngest/pkg/execution/state"
	"github.com/khulnasoft/inngest/pkg/execution/state/redis_state"
	"github.com/khulnasoft/inngest/pkg/expressions"
	"github.com/khulnasoft/inngest/pkg/inngest"
	"github.com/khulnasoft/inngest/pkg/inngest/log"
	"github.com/khulnasoft/inngest/pkg/logger"
	"github.com/oklog/ulid/v2"
	"github.com/redis/rueidis"
	"github.com/xhit/go-str2duration/v2"
)

//go:embed lua/*
var embedded embed.FS

var (
	ErrDebounceExists     = fmt.Errorf("a debounce exists for this function")
	ErrDebounceNotFound   = fmt.Errorf("debounce not found")
	ErrDebounceInProgress = fmt.Errorf("debounce is in progress")
)

var (
	buffer = 50 * time.Millisecond
	// scripts stores all embedded lua scripts on initialization
	scripts = map[string]*rueidis.Lua{}
	include = regexp.MustCompile(`-- \$include\(([\w.]+)\)`)
)

func init() {
	// read the lua scripts
	entries, err := embedded.ReadDir("lua")
	if err != nil {
		panic(fmt.Errorf("error reading redis lua dir: %w", err))
	}
	readRedisScripts("lua", entries)
}

// The general strategy for debounce:
//
// 1. Create a new debounce key.
// 2. Store the current event in the debounce key.
// 3. Create a new queue item for the debounce, linking to the debounce key

// DebounceItem represents a debounce stored within the debounce manager.
//
// DebounceItem fulfils event.TrackedEvent, allowing the use of the entire DebounceItem
// as the triggering event data passed to executor.Schedule.
type DebounceItem struct {
	// AccountID represents the account for the debounce item
	AccountID uuid.UUID `json:"aID"`
	// WorkspaceID represents the workspace for the debounce item
	WorkspaceID uuid.UUID `json:"wsID"`
	// AppID represents the app for the debounce item
	AppID uuid.UUID `json:"appID"`
	// FunctionID represents the function ID that this debounce is for.
	FunctionID uuid.UUID `json:"fnID"`
	// FunctionVersion represents the version of the function that was debounced.
	FunctionVersion int `json:"fnV"`
	// EventID represents the internal event ID that triggers the function.
	EventID ulid.ULID `json:"eID"`
	// Event represents the event data which triggers the function.
	Event event.Event `json:"e"`
	// Timeout is the timeout for the debounce, in unix milliseconds.
	Timeout int64 `json:"t,omitempty"`
	// FunctionPausedAt indicates whether the function is paused.
	FunctionPausedAt *time.Time `json:"fpAt,omitempty"`
}

func (d DebounceItem) QueuePayload() DebouncePayload {
	return DebouncePayload{
		AccountID:       d.AccountID,
		WorkspaceID:     d.WorkspaceID,
		AppID:           d.AppID,
		FunctionID:      d.FunctionID,
		FunctionVersion: d.FunctionVersion,
	}
}

func (d DebounceItem) GetInternalID() ulid.ULID {
	return d.EventID
}

func (d DebounceItem) GetEvent() event.Event {
	return d.Event
}

func (d DebounceItem) GetWorkspaceID() uuid.UUID {
	return d.WorkspaceID
}

// DebouncePayload represents the data stored within the queue's payload.
type DebouncePayload struct {
	DebounceID ulid.ULID `json:"debounceID"`
	// AccountID represents the account for the debounce item
	AccountID uuid.UUID `json:"aID"`
	// WorkspaceID represents the workspace for the debounce item
	WorkspaceID uuid.UUID `json:"wsID"`
	// AppID represents the app for the debounce item
	AppID uuid.UUID `json:"appID"`
	// FunctionID represents the function ID that this debounce is for.
	FunctionID uuid.UUID `json:"fnID"`
	// FunctionVersion represents the version of the function that was debounced.
	FunctionVersion int `json:"fnV"`
}

// Debouncer represents an implementation-agnostic function debouncer, delaying function runs
// until a specific time period passes when no more events matching a key are received.
type Debouncer interface {
	Debounce(ctx context.Context, d DebounceItem, fn inngest.Function) error
	GetDebounceItem(ctx context.Context, debounceID ulid.ULID) (*DebounceItem, error)
	DeleteDebounceItem(ctx context.Context, debounceID ulid.ULID) error
	StartExecution(ctx context.Context, d DebounceItem, fn inngest.Function, debounceID ulid.ULID) error
}

func NewRedisDebouncer(d *redis_state.DebounceClient, defaultQueueShard redis_state.QueueShard, q redis_state.QueueManager) Debouncer {
	return debouncer{
		d:                 d,
		q:                 q,
		defaultQueueShard: defaultQueueShard,
	}
}

type debouncer struct {
	d                 *redis_state.DebounceClient
	q                 redis_state.QueueManager
	defaultQueueShard redis_state.QueueShard
}

// DeleteDebounceItem removes a debounce from the map.
func (d debouncer) DeleteDebounceItem(ctx context.Context, debounceID ulid.ULID) error {
	keyDbc := d.d.KeyGenerator().Debounce(ctx)
	cmd := d.d.Client().B().Hdel().Key(keyDbc).Field(debounceID.String()).Build()
	err := d.d.Client().Do(ctx, cmd).Error()
	if rueidis.IsRedisNil(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("error removing debounce: %w", err)
	}
	return nil
}

// GetDebounceItem returns a DebounceItem given a debounce ID.
func (d debouncer) GetDebounceItem(ctx context.Context, debounceID ulid.ULID) (*DebounceItem, error) {
	keyDbc := d.d.KeyGenerator().Debounce(ctx)

	cmd := d.d.Client().B().Hget().Key(keyDbc).Field(debounceID.String()).Build()
	byt, err := d.d.Client().Do(ctx, cmd).AsBytes()
	if rueidis.IsRedisNil(err) {
		return nil, ErrDebounceNotFound
	}

	di := &DebounceItem{}
	if err := json.Unmarshal(byt, &di); err != nil {
		return nil, fmt.Errorf("error unmarshalling debounce item: %w", err)
	}
	return di, nil
}

// StartExecution swaps out the underlying pointer of the debounce
func (d debouncer) StartExecution(ctx context.Context, di DebounceItem, fn inngest.Function, debounceID ulid.ULID) error {
	dkey, err := d.debounceKey(ctx, di, fn)
	if err != nil {
		return err
	}

	newDebounceID := ulid.MustNew(ulid.Now(), rand.Reader)

	keys := []string{d.d.KeyGenerator().DebouncePointer(ctx, fn.ID, dkey)}
	args := []string{
		newDebounceID.String(),
		debounceID.String(),
	}

	res, err := scripts["start"].Exec(
		ctx,
		d.d.Client(),
		keys,
		args,
	).AsInt64()
	if err != nil {
		return err
	}

	switch res {
	case 0, 1:
		return nil
	default:
		return fmt.Errorf("invalid status returned when starting debounce: %d", res)
	}
}

// Debounce debounces a given function with the given DebounceItem.
func (d debouncer) Debounce(ctx context.Context, di DebounceItem, fn inngest.Function) error {
	if fn.Debounce == nil {
		return fmt.Errorf("fn has no debounce config")
	}
	ttl, err := str2duration.ParseDuration(fn.Debounce.Period)
	if err != nil {
		return fmt.Errorf("invalid debounce duration: %w", err)
	}
	return d.debounce(ctx, di, fn, ttl, 0)
}

func (d debouncer) debounce(ctx context.Context, di DebounceItem, fn inngest.Function, ttl time.Duration, n int) error {
	// Call new debounce immediately.  If this returns ErrDebounceExists then
	// update the debounce.  This ensures that checking and creating a debounce
	// is atomic, and two individual threads/workers cannot create debounces simultaneously.
	debounceID, err := d.newDebounce(ctx, di, fn, ttl)
	if err == nil {
		return nil
	}
	if err != ErrDebounceExists {
		// There was an unkown error creating the debounce.
		return err
	}
	if debounceID == nil {
		return fmt.Errorf("expected debounce ID when debounce exists")
	}

	// A debounce must already exist for this fn.  Update it.
	err = d.updateDebounce(ctx, di, fn, ttl, *debounceID)
	if err == context.DeadlineExceeded || err == ErrDebounceInProgress || err == ErrDebounceNotFound {
		if n == 5 {
			logger.StdlibLogger(ctx).Error("unable to update debounce", "error", err)
			// Only recurse 5 times.
			return fmt.Errorf("unable to update debounce: %w", err)
		}
		// Re-invoke this to see if we need to extend the debounce or continue.
		// Wait 50 milliseconds for the current lock and job to have evaluated.
		//
		// TODO: Instead of this, make debounce creation and updating atomic within the queue.
		// This needs to modify queue items and partitions directly.
		<-time.After(750 * time.Millisecond)
		return d.debounce(ctx, di, fn, ttl, n+1)
	}

	return err
}

func (d debouncer) queueItem(ctx context.Context, di DebounceItem, debounceID ulid.ULID) queue.Item {
	jobID := debounceID.String()
	payload := di.QueuePayload()
	payload.DebounceID = debounceID
	return queue.Item{
		JobID:       &jobID,
		WorkspaceID: di.WorkspaceID,
		Identifier: state.Identifier{
			AccountID:   di.AccountID,
			WorkspaceID: di.WorkspaceID,
			AppID:       di.AppID,
			WorkflowID:  di.FunctionID,
		},
		Kind:    queue.KindDebounce,
		Payload: payload,
	}
}

func (d debouncer) newDebounce(ctx context.Context, di DebounceItem, fn inngest.Function, ttl time.Duration) (*ulid.ULID, error) {
	now := time.Now()
	debounceID := ulid.MustNew(ulid.Now(), rand.Reader)

	key, err := d.debounceKey(ctx, di, fn)
	if err != nil {
		return nil, err
	}

	// Ensure we set the debounce's max lifetime.
	if timeout := fn.Debounce.TimeoutDuration(); timeout != nil {
		di.Timeout = time.Now().Add(*timeout).UnixMilli()
	}

	keyPtr := d.d.KeyGenerator().DebouncePointer(ctx, fn.ID, key)
	keyDbc := d.d.KeyGenerator().Debounce(ctx)

	byt, err := json.Marshal(di)
	if err != nil {
		return nil, fmt.Errorf("error marshalling debounce: %w", err)
	}

	out, err := scripts["newDebounce"].Exec(
		ctx,
		d.d.Client(),
		[]string{keyPtr, keyDbc},
		[]string{debounceID.String(), string(byt), strconv.Itoa(int(ttl.Seconds()))},
	).ToString()
	if err != nil {
		return nil, fmt.Errorf("error creating debounce: %w", err)
	}

	if out == "0" {
		// Enqueue the debounce job with extra buffer.  This ensures that we never
		// attempt to start a debounce during the debounce's expiry (race conditions), and the extra
		// second lets an updateDebounce call on TTL 0 finish, as the buffer is the updateDebounce
		// deadline.
		qi := d.queueItem(ctx, di, debounceID)
		err = d.q.Enqueue(ctx, qi, now.Add(ttl).Add(buffer).Add(time.Second), queue.EnqueueOpts{})
		if err != nil {
			return &debounceID, fmt.Errorf("error enqueueing debounce job: %w", err)
		}
		return &debounceID, nil
	}

	existingID, err := ulid.Parse(out)
	if err != nil {
		// This was not a ULID, so we have no idea what was returned.
		return nil, fmt.Errorf("unknown new debounce return value: %s", out)
	}
	return &existingID, ErrDebounceExists
}

// updateDebounce updates the currently pending debounce to point to the new event ID.  It pushes
// out the debounce's TTL, and re-enqueues the job to initialize fns from the debounce.
func (d debouncer) updateDebounce(ctx context.Context, di DebounceItem, fn inngest.Function, ttl time.Duration, debounceID ulid.ULID) error {
	now := time.Now()

	key, err := d.debounceKey(ctx, di, fn)
	if err != nil {
		return err
	}

	// NOTE: This function has a deadline to complete.  If this fn doesn't complete within the deadline,
	// eg, network issues, we must check if the debounce expired and re-attempt the entire thing, allowing
	// us to either update or create a new debounce depending on the current time.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	keyPtr := d.d.KeyGenerator().DebouncePointer(ctx, fn.ID, key)
	keyDbc := d.d.KeyGenerator().Debounce(ctx)
	byt, err := json.Marshal(di)
	if err != nil {
		return fmt.Errorf("error marshalling debounce: %w", err)
	}

	out, err := scripts["updateDebounce"].Exec(
		ctx,
		d.d.Client(),
		[]string{
			keyPtr,
			keyDbc,
			d.d.KeyGenerator().QueueItem(),
		},
		[]string{
			debounceID.String(),
			string(byt),
			strconv.Itoa(int(ttl.Seconds())),
			queue.HashID(ctx, debounceID.String()),
			strconv.Itoa(int(time.Now().UnixMilli())),
			strconv.Itoa(int(di.Event.Timestamp)),
		},
	).AsInt64()
	if err != nil {
		return fmt.Errorf("error creating debounce: %w", err)
	}
	switch out {
	case -1:
		// The debounce is in progress or has just finished.  Requeue.
		return ErrDebounceInProgress
	case -2:
		// The event is out-of-order and a newer event exists within the debounce.
		// Do nothing.
		return nil
	case -3:
		// The item is not found with the debounceID
		// enqueue a new item
		qi := d.queueItem(ctx, di, debounceID)
		return d.q.Enqueue(ctx, qi, now.Add(ttl).Add(buffer).Add(time.Second), queue.EnqueueOpts{})
	default:
		// Debounces should have a maximum timeout;  updating the debounce returns
		// the timeout to use.
		actualTTL := time.Second * time.Duration(out)
		err = d.q.RequeueByJobID(
			ctx,
			d.defaultQueueShard,
			debounceID.String(),
			now.Add(actualTTL).Add(buffer).Add(time.Second),
		)
		if err == redis_state.ErrQueueItemAlreadyLeased {
			log.From(ctx).Warn().
				Str("err", err.Error()).
				Int64("ttl", out).
				Msg(ErrDebounceInProgress.Error())
			// This is in progress.
			return ErrDebounceInProgress
		}
		if err != nil {
			return fmt.Errorf("error requeueing debounce job '%s': %w", debounceID, err)
		}
		return nil
	}
}

func (d debouncer) debounceKey(ctx context.Context, evt event.TrackedEvent, fn inngest.Function) (string, error) {
	if fn.Debounce.Key == nil {
		return fn.ID.String(), nil
	}

	out, _, err := expressions.Evaluate(ctx, *fn.Debounce.Key, map[string]any{"event": evt.GetEvent().Map()})
	if err != nil {
		log.From(ctx).Error().Err(err).
			Str("expression", *fn.Debounce.Key).
			Interface("event", evt.GetEvent().Map()).
			Msg("error evaluating debounce expression")
		return "<invalid>", nil
	}
	if str, ok := out.(string); ok {
		return str, nil
	}
	return fmt.Sprintf("%v", out), nil
}

func readRedisScripts(path string, entries []fs.DirEntry) {
	for _, e := range entries {
		// NOTE: When using embed go always uses forward slashes as a path
		// prefix. filepath.Join uses OS-specific prefixes which fails on
		// windows, so we construct the path using Sprintf for all platforms
		if e.IsDir() {
			entries, _ := embedded.ReadDir(fmt.Sprintf("%s/%s", path, e.Name()))
			readRedisScripts(path+"/"+e.Name(), entries)
			continue
		}

		byt, err := embedded.ReadFile(fmt.Sprintf("%s/%s", path, e.Name()))
		if err != nil {
			panic(fmt.Errorf("error reading redis lua script: %w", err))
		}

		name := path + "/" + e.Name()
		name = strings.TrimPrefix(name, "lua/")
		name = strings.TrimSuffix(name, ".lua")
		val := string(byt)

		// Add any includes.
		items := include.FindAllStringSubmatch(val, -1)
		if len(items) > 0 {
			// Replace each include
			for _, include := range items {
				byt, err = embedded.ReadFile(fmt.Sprintf("lua/includes/%s", include[1]))
				if err != nil {
					panic(fmt.Errorf("error reading redis lua include: %w", err))
				}
				val = strings.ReplaceAll(val, include[0], string(byt))
			}
		}
		scripts[name] = rueidis.NewLuaScript(val)
	}
}
