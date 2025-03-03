package pubsub

import (
	"context"
	"github.com/khulnasoft/inngest/pkg/logger"
	"github.com/oklog/ulid/v2"

	"github.com/google/uuid"
	connpb "github.com/khulnasoft/inngest/proto/gen/connect/v1"
)

// noopConnector is a blank implementation of the Connector interface
type noopConnector struct{}

func (noopConnector) Proxy(ctx context.Context, appID uuid.UUID, data *connpb.GatewayExecutorRequestData) (*connpb.SDKResponse, error) {
	logger.StdlibLogger(ctx).Error("using no-op connector to proxy message", "data", data)

	return &connpb.SDKResponse{}, nil
}

func (noopConnector) ReceiveExecutorMessages(ctx context.Context, onMessage func(byt []byte, data *connpb.GatewayExecutorRequestData)) error {
	logger.StdlibLogger(ctx).Error("using no-op connector to receive executor messages")

	return nil
}

func (noopConnector) RouteExecutorRequest(ctx context.Context, gatewayId ulid.ULID, appId uuid.UUID, connId ulid.ULID, data *connpb.GatewayExecutorRequestData) error {
	logger.StdlibLogger(ctx).Error("using no-op connector to forward executor request to gateway", "gateway_id", gatewayId, "app_id", appId, "conn_id", connId)

	return nil
}

func (noopConnector) ReceiveRoutedRequest(ctx context.Context, gatewayId ulid.ULID, appId uuid.UUID, connId ulid.ULID, onMessage func(rawBytes []byte, data *connpb.GatewayExecutorRequestData)) error {
	logger.StdlibLogger(ctx).Error("using no-op connector receive routed request", "gateway_id", gatewayId, "app_id", appId, "conn_id", connId)

	return nil
}

func (noopConnector) AckMessage(ctx context.Context, appId uuid.UUID, requestId string, source AckSource) error {
	logger.StdlibLogger(ctx).Error("using no-op connector to ack message", "request_id", requestId, "source", source)

	return nil
}

func (noopConnector) NotifyExecutor(ctx context.Context, resp *connpb.SDKResponse) error {
	logger.StdlibLogger(ctx).Error("using no-op connector to notify executor", "resp", resp)

	return nil
}

func (noopConnector) Wait(ctx context.Context) error {
	return nil
}
