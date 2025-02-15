package memory_store

import (
	"sync"

	"github.com/khulnasoft/inngest/pkg/execution/history"
	"github.com/khulnasoft/inngest/pkg/history_reader"
	"github.com/oklog/ulid/v2"
)

var (
	Singleton = &RunStore{
		Data: map[ulid.ULID]RunData{},
		Mu:   &sync.RWMutex{},
	}
)

type RunData struct {
	Run     history_reader.Run
	History []history.History
}

type RunStore struct {
	Data map[ulid.ULID]RunData
	Mu   *sync.RWMutex
}
