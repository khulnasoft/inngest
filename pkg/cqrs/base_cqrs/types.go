package base_cqrs

import (
	sqlc "github.com/khulnasoft/inngest/pkg/cqrs/base_cqrs/sqlc/sqlite"
	"github.com/khulnasoft/inngest/pkg/execution/history"
	"github.com/jinzhu/copier"
)

func convertHistoryToWriter(h history.History) (*sqlc.InsertHistoryParams, error) {
	to := sqlc.InsertHistoryParams{}
	if err := copier.CopyWithOption(&to, h, copier.Option{DeepCopy: true}); err != nil {
		return nil, err
	}

	return &to, nil
}
