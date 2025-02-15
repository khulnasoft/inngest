package resolvers

import (
	"context"

	"github.com/google/uuid"
	"github.com/khulnasoft/inngest/pkg/coreapi/graph/models"
	"github.com/khulnasoft/inngest/pkg/cqrs"
)

func (r *functionResolver) App(ctx context.Context, obj *models.Function) (*cqrs.App, error) {
	appID := uuid.MustParse(obj.AppID)
	return r.Data.GetAppByID(ctx, appID)
}
