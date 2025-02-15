package defaults

import (
	// Import the default drivers, queues, and state stores.
	_ "github.com/khulnasoft/inngest/pkg/execution/driver/connectdriver"
	_ "github.com/khulnasoft/inngest/pkg/execution/driver/httpdriver"
	_ "github.com/khulnasoft/inngest/pkg/execution/driver/mockdriver"
	_ "github.com/khulnasoft/inngest/pkg/execution/state/redis_state"
)
