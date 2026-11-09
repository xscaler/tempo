package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/dskit/middleware"
	"github.com/grafana/dskit/user"
	"github.com/stretchr/testify/require"
	"github.com/xscaler/dskit/xstenant"

	"github.com/grafana/tempo/modules/overrides"
)

// authOverrides answers CheckAuth the way Reader.CheckAuth does for one key ("good" for tenant
// "t-a") under a mode. The embedded Interface is never called.
type authOverrides struct {
	overrides.Interface
	mode xstenant.EnforceMode
	reqs []xstenant.AuthRequest
}

func (o *authOverrides) CheckAuth(req xstenant.AuthRequest) xstenant.AuthDecision {
	o.reqs = append(o.reqs, req)
	if o.mode != xstenant.EnforceShadow && o.mode != xstenant.EnforceOn {
		return xstenant.AuthDecision{Allowed: true}
	}
	path, _, _ := strings.Cut(req.Path, "?")
	var d xstenant.AuthDecision
	switch {
	case req.Authorization != "Bearer good":
		d = xstenant.AuthDecision{Status: http.StatusUnauthorized, Message: "unknown token"}
	case path != "/api/search" && path != "/tempo/api/search":
		d = xstenant.AuthDecision{Status: http.StatusForbidden, Message: "unsupported path"}
	case req.OrgID != "t-a":
		d = xstenant.AuthDecision{Status: http.StatusUnauthorized, Message: "x-scope-orgid mismatch"}
	default:
		d = xstenant.AuthDecision{Allowed: true, Tenant: "t-a"}
	}
	if !d.Allowed && o.mode == xstenant.EnforceShadow {
		d.Allowed = true
	}
	d.Enforced = d.Allowed && d.Tenant != "" && o.mode == xstenant.EnforceOn
	return d
}

func TestWithAPIKeyAuth(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       xstenant.EnforceMode
		path       string
		auth       string
		org        string
		wantStatus int
		wantTenant string
		wantBody   string
	}{
		{"off", xstenant.EnforceOff, "/api/search", "Bearer nope", "t-b", http.StatusOK, "t-b", ""},
		{"shadow bad key is served", xstenant.EnforceShadow, "/api/search", "Bearer nope", "t-b", http.StatusOK, "t-b", ""},
		{"shadow good key keeps the sent tenant", xstenant.EnforceShadow, "/api/search", "Bearer good", "t-b", http.StatusOK, "t-b", ""},
		{"on good key", xstenant.EnforceOn, "/api/search", "Bearer good", "t-a", http.StatusOK, "t-a", ""},
		{"on bad key", xstenant.EnforceOn, "/api/search", "Bearer nope", "t-a", http.StatusUnauthorized, "", "unknown token"},
		{"on mismatch", xstenant.EnforceOn, "/api/search", "Bearer good", "t-b", http.StatusUnauthorized, "", "x-scope-orgid mismatch"},
		{"on path not allowed", xstenant.EnforceOn, "/api/overrides", "Bearer good", "t-a", http.StatusForbidden, "", "unsupported path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &authOverrides{mode: tc.mode}
			var gotTenant string
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotTenant, _ = user.ExtractOrgID(r.Context())
			})
			h := withAPIKeyAuth(o, "", middleware.AuthenticateUser).Wrap(next)

			r := httptest.NewRequest(http.MethodGet, tc.path+"?q=1", nil)
			r.Header.Set("Authorization", tc.auth)
			r.Header.Set(user.OrgIDHeaderName, tc.org)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			require.Equal(t, tc.wantStatus, w.Code)
			require.Equal(t, tc.wantTenant, gotTenant)
			if tc.wantBody != "" {
				require.Contains(t, w.Body.String(), tc.wantBody)
			}
			require.Len(t, o.reqs, 1)
			require.Equal(t, xstenant.KindTraces, o.reqs[0].Kind)
			require.Equal(t, tc.path+"?q=1", o.reqs[0].Path)
			// the caller's header is not modified
			require.Equal(t, tc.org, r.Header.Get(user.OrgIDHeaderName))
		})
	}
}

func TestWithAPIKeyAuthReplacesTenant(t *testing.T) {
	// the key's tenant wins over the header in mode on
	o := &enforcingOverrides{tenant: "t-key"}
	var gotTenant string
	h := withAPIKeyAuth(o, "/tempo", middleware.AuthenticateUser).Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotTenant, _ = user.ExtractOrgID(r.Context())
	}))
	r := httptest.NewRequest(http.MethodGet, "/tempo/api/search", nil)
	r.Header.Set(user.OrgIDHeaderName, "t-sent")
	h.ServeHTTP(httptest.NewRecorder(), r)
	require.Equal(t, "t-key", gotTenant)
	require.Equal(t, "/tempo", o.req.PathPrefix)
}

type enforcingOverrides struct {
	overrides.Interface
	tenant string
	req    xstenant.AuthRequest
}

func (o *enforcingOverrides) CheckAuth(req xstenant.AuthRequest) xstenant.AuthDecision {
	o.req = req
	return xstenant.AuthDecision{Allowed: true, Enforced: true, Tenant: o.tenant}
}

// Overrides that do not read the admin store never block and never change the tenant.
func TestWithAPIKeyAuthWithoutAdminStore(t *testing.T) {
	var gotTenant string
	h := withAPIKeyAuth(overrides.Interface(nil), "", middleware.AuthenticateUser).Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotTenant, _ = user.ExtractOrgID(r.Context())
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/search", nil)
	r.Header.Set(user.OrgIDHeaderName, "t-sent")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "t-sent", gotTenant)
}
