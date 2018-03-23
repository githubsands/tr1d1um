package common

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Comcast/webpa-common/logging"
	kitlog "github.com/go-kit/kit/log"
	kithttp "github.com/go-kit/kit/transport/http"
)

//TransactionLogging is used by the different Tr1d1um services to
//keep track of incoming requests and their corresponding responses
func TransactionLogging(logger kitlog.Logger) kithttp.ServerFinalizerFunc {
	return func(ctx context.Context, code int, r *http.Request) {

		transactionLogger := kitlog.WithPrefix(logging.Info(logger),
			logging.MessageKey(), "Bookkeeping response",
			"requestAddress", r.RemoteAddr,
			"requestURLPath", r.URL.Path,
			"requestURLQuery", r.URL.RawQuery,
			"requestMethod", r.Method,
			"responseCode", code,
			"responseHeaders", ctx.Value(kithttp.ContextKeyResponseHeaders),
			"responseError", ctx.Value(ContextKeyResponseError),
		)

		var latency = "-"

		if requestArrivalTime, ok := ctx.Value(ContextKeyRequestArrivalTime).(time.Time); ok {
			latency = fmt.Sprintf("%v", time.Now().Sub(requestArrivalTime))
		} else {
			logging.Error(logger).Log(logging.ErrorKey(), "latency value could not be derived")
		}

		transactionLogger.Log("latency", latency)
	}
}

//ForwardHeadersByPrefix copies headers h from resp to w such that key(h) has p as prefix
func ForwardHeadersByPrefix(p string, resp *http.Response, w http.ResponseWriter) {
	if resp != nil {
		for headerKey, headerValues := range resp.Header {
			if strings.HasPrefix(headerKey, p) {
				for _, headerValue := range headerValues {
					w.Header().Add(headerKey, headerValue)
				}
			}
		}
	}
}