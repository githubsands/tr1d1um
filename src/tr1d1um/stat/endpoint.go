package stat

import (
	"context"

	money "github.com/Comcast/golang-money"
	"github.com/go-kit/kit/endpoint"
)

type statRequest struct {
	DeviceID        string
	AuthHeaderValue string
	httpTracker     *money.HTTPTracker
}

func makeStatEndpoint(s Service) endpoint.Endpoint {
	return func(ctx context.Context, r interface{}) (interface{}, error) {
		statReq := (r).(*statRequest)
		return s.RequestStat(statReq.AuthHeaderValue, statReq.DeviceID)
	}
}
