package app

import (
	"net/http"

	"github.com/grafana/dskit/middleware"
	"github.com/grafana/dskit/user"
	"github.com/xscaler/dskit/xstenant"

	"github.com/grafana/tempo/modules/overrides"
)

// withAPIKeyAuth returns a middleware that authorizes the request against the published API keys
// and then runs auth, the middleware that extracts the tenant. It is for the HTTP API served to
// clients; routes the components call each other on must not use it. With an enforced decision
// the tenant header is replaced by the key's tenant before auth reads it. When o does not read
// the admin store, or enforce_auth is off, requests pass through unchanged.
func withAPIKeyAuth(o overrides.Interface, apiPrefix string, auth middleware.Interface) middleware.Interface {
	return middleware.Func(func(next http.Handler) http.Handler {
		inner := auth.Wrap(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d := overrides.CheckAuth(o, xstenant.AuthRequest{
				Kind:          xstenant.KindTraces,
				Method:        r.Method,
				Path:          r.URL.RequestURI(),
				PathPrefix:    apiPrefix,
				Authorization: r.Header.Get("Authorization"),
				OrgID:         r.Header.Get(user.OrgIDHeaderName),
			})
			if !d.Allowed {
				http.Error(w, d.Message, d.Status)
				return
			}
			if d.Enforced {
				r = r.Clone(r.Context())
				r.Header.Set(user.OrgIDHeaderName, d.Tenant)
			}
			inner.ServeHTTP(w, r)
		})
	})
}
