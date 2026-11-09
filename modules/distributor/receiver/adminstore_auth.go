package receiver

import (
	"context"
	"net/http"
	"strings"

	"github.com/grafana/dskit/user"
	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/xscaler/dskit/xstenant"
)

const (
	// otlpHTTPTracesPath is checked for an OTLP HTTP request. /v1/traces and /otlp/v1/traces share
	// the same rules, and the consumer does not see which one the client used.
	otlpHTTPTracesPath = "/v1/traces"
	otlpGRPCExportPath = "/opentelemetry.proto.collector.trace.v1.TraceService/Export"
)

// Authorizer decides one request against the published API keys.
type Authorizer func(req xstenant.AuthRequest) xstenant.AuthDecision

// Option configures New.
type Option func(*options)

type options struct {
	otlpAuth Authorizer
}

// WithOTLPAuthorizer checks API keys on the OTLP receiver. Other receivers are not affected.
func WithOTLPAuthorizer(a Authorizer) Option {
	return func(o *options) { o.otlpAuth = a }
}

// authMiddleware authorizes a request before the tenant is extracted from it. With an enforced
// decision the tenant header is replaced by the key's tenant, so the tenant middleware that runs
// next reads it.
type authMiddleware struct {
	check Authorizer
}

func (m *authMiddleware) Wrap(next consumer.Traces) consumer.Traces {
	return ConsumeTracesFunc(func(ctx context.Context, td ptrace.Traces) error {
		md, isGRPC := metadata.FromIncomingContext(ctx)
		req := xstenant.AuthRequest{Kind: xstenant.KindTraces, Method: http.MethodPost}
		if isGRPC {
			req.Path = otlpGRPCExportPath
			req.Authorization = strings.Join(md.Get("authorization"), " ")
			req.OrgID = strings.Join(md.Get(strings.ToLower(user.OrgIDHeaderName)), ",")
		} else {
			info := client.FromContext(ctx)
			req.Path = otlpHTTPTracesPath
			req.Authorization = strings.Join(info.Metadata.Get("Authorization"), " ")
			req.OrgID = strings.Join(info.Metadata.Get(user.OrgIDHeaderName), ",")
		}

		d := m.check(req)
		if !d.Allowed {
			return status.Error(grpcCode(d.Status), d.Message)
		}
		if d.Enforced {
			if isGRPC {
				md = md.Copy()
				md.Set(strings.ToLower(user.OrgIDHeaderName), d.Tenant)
				ctx = metadata.NewIncomingContext(ctx, md)
			} else {
				info := client.FromContext(ctx)
				kv := map[string][]string{}
				for k := range info.Metadata.Keys() {
					kv[k] = info.Metadata.Get(k)
				}
				kv[strings.ToLower(user.OrgIDHeaderName)] = []string{d.Tenant}
				info.Metadata = client.NewMetadata(kv)
				ctx = client.NewContext(ctx, info)
			}
		}
		return next.ConsumeTraces(ctx, td)
	})
}

// grpcCode maps the HTTP status of a denial to a gRPC code. The OTLP HTTP receiver maps the code
// back to the same HTTP status.
func grpcCode(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	default:
		return codes.PermissionDenied
	}
}
