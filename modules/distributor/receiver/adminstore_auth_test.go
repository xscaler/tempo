package receiver

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	dslog "github.com/grafana/dskit/log"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/user"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/xscaler/dskit/xstenant"
	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/pdata/testdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/grafana/tempo/pkg/tempopb"
)

// modeAuthorizer mimics Reader.CheckAuth for one key ("good" for tenant "t-a") under a mode and
// records every request it is asked about.
type modeAuthorizer struct {
	mode xstenant.EnforceMode

	mu   sync.Mutex
	reqs []xstenant.AuthRequest
}

func (a *modeAuthorizer) check(req xstenant.AuthRequest) xstenant.AuthDecision {
	a.mu.Lock()
	a.reqs = append(a.reqs, req)
	a.mu.Unlock()

	if a.mode != xstenant.EnforceShadow && a.mode != xstenant.EnforceOn {
		return xstenant.AuthDecision{Allowed: true}
	}
	var d xstenant.AuthDecision
	switch {
	case req.Authorization != "Bearer good":
		d = xstenant.AuthDecision{Status: http.StatusUnauthorized, Reason: xstenant.ReasonUnknownToken, Message: "unknown token"}
	case req.OrgID != "t-a":
		d = xstenant.AuthDecision{Status: http.StatusUnauthorized, Reason: xstenant.ReasonScopeOrgIDMismatch, Message: "x-scope-orgid mismatch"}
	default:
		d = xstenant.AuthDecision{Allowed: true, Tenant: "t-a", Reason: xstenant.ReasonAuthAllowed}
	}
	if !d.Allowed && a.mode == xstenant.EnforceShadow {
		d.Allowed = true
	}
	d.Enforced = d.Allowed && d.Tenant != "" && a.mode == xstenant.EnforceOn
	return d
}

func (a *modeAuthorizer) requests() []xstenant.AuthRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]xstenant.AuthRequest(nil), a.reqs...)
}

func TestAuthMiddlewareHTTPContext(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       xstenant.EnforceMode
		auth       string
		orgID      string
		wantErr    codes.Code
		wantTenant string
	}{
		{"off ignores a bad key", xstenant.EnforceOff, "Bearer nope", "t-b", codes.OK, "t-b"},
		{"shadow passes a bad key and keeps the tenant", xstenant.EnforceShadow, "Bearer nope", "t-b", codes.OK, "t-b"},
		{"shadow keeps the sent tenant for a good key", xstenant.EnforceShadow, "Bearer good", "t-b", codes.OK, "t-b"},
		{"on replaces nothing when the tenant matches", xstenant.EnforceOn, "Bearer good", "t-a", codes.OK, "t-a"},
		{"on denies a bad key", xstenant.EnforceOn, "Bearer nope", "t-a", codes.Unauthenticated, ""},
		{"on denies a tenant mismatch", xstenant.EnforceOn, "Bearer good", "t-b", codes.Unauthenticated, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &modeAuthorizer{mode: tc.mode}
			var gotTenant string
			next := ConsumeTracesFunc(func(ctx context.Context, _ ptrace.Traces) error {
				id, err := user.ExtractOrgID(ctx)
				require.NoError(t, err)
				gotTenant = id
				return nil
			})
			h := (&authMiddleware{check: a.check}).Wrap(MultiTenancyMiddleware().Wrap(next))

			err := h.ConsumeTraces(httpCtx(tc.auth, tc.orgID), ptrace.NewTraces())
			if tc.wantErr != codes.OK {
				require.Equal(t, tc.wantErr, status.Code(err))
				require.Empty(t, gotTenant)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantTenant, gotTenant)
			}
			reqs := a.requests()
			require.Len(t, reqs, 1)
			require.Equal(t, xstenant.KindTraces, reqs[0].Kind)
			require.Equal(t, "/v1/traces", reqs[0].Path)
			require.Equal(t, http.MethodPost, reqs[0].Method)
			require.Equal(t, tc.auth, reqs[0].Authorization)
			require.Equal(t, tc.orgID, reqs[0].OrgID)
		})
	}
}

func TestAuthMiddlewareReplacesTenant(t *testing.T) {
	// a check that enforces a tenant different from the one sent
	check := func(xstenant.AuthRequest) xstenant.AuthDecision {
		return xstenant.AuthDecision{Allowed: true, Enforced: true, Tenant: "t-key"}
	}
	var got string
	next := ConsumeTracesFunc(func(ctx context.Context, _ ptrace.Traces) error {
		got, _ = user.ExtractOrgID(ctx)
		return nil
	})
	h := (&authMiddleware{check: check}).Wrap(MultiTenancyMiddleware().Wrap(next))

	require.NoError(t, h.ConsumeTraces(httpCtx("Bearer good", "t-sent"), ptrace.NewTraces()))
	require.Equal(t, "t-key", got)

	got = ""
	grpcCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer good", "x-scope-orgid", "t-sent"))
	require.NoError(t, h.ConsumeTraces(grpcCtx, ptrace.NewTraces()))
	require.Equal(t, "t-key", got)
}

func TestAuthMiddlewareDenialCodes(t *testing.T) {
	for httpStatus, want := range map[int]codes.Code{
		http.StatusUnauthorized:       codes.Unauthenticated,
		http.StatusForbidden:          codes.PermissionDenied,
		http.StatusServiceUnavailable: codes.Unavailable,
	} {
		check := func(xstenant.AuthRequest) xstenant.AuthDecision {
			return xstenant.AuthDecision{Status: httpStatus, Message: "denied"}
		}
		h := (&authMiddleware{check: check}).Wrap(ConsumeTracesFunc(func(context.Context, ptrace.Traces) error {
			t.Fatal("next must not run")
			return nil
		}))
		err := h.ConsumeTraces(httpCtx("", ""), ptrace.NewTraces())
		require.Equal(t, want, status.Code(err))
		require.Equal(t, "denied", status.Convert(err).Message())
	}
}

func TestAuthMiddlewareGRPCRequest(t *testing.T) {
	a := &modeAuthorizer{mode: xstenant.EnforceOn}
	h := (&authMiddleware{check: a.check}).Wrap(ConsumeTracesFunc(func(context.Context, ptrace.Traces) error { return nil }))
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer good", "x-scope-orgid", "t-a"))
	require.NoError(t, h.ConsumeTraces(ctx, ptrace.NewTraces()))

	reqs := a.requests()
	require.Len(t, reqs, 1)
	require.Equal(t, "/opentelemetry.proto.collector.trace.v1.TraceService/Export", reqs[0].Path)
	require.Equal(t, "Bearer good", reqs[0].Authorization)
	require.Equal(t, "t-a", reqs[0].OrgID)

	// a request without a tenant header is still checked, not skipped
	err := h.ConsumeTraces(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer good")), ptrace.NewTraces())
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

type tenantPusher struct {
	mu      sync.Mutex
	tenants []string
}

func (p *tenantPusher) PushTraces(ctx context.Context, _ ptrace.Traces) (*tempopb.PushResponse, error) {
	id, _ := user.ExtractOrgID(ctx)
	p.mu.Lock()
	p.tenants = append(p.tenants, id)
	p.mu.Unlock()
	return &tempopb.PushResponse{}, nil
}

func (p *tenantPusher) RetryInfoEnabled(context.Context) (bool, error) { return false, nil }

func (p *tenantPusher) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tenants...)
}

func runAuthShim(t *testing.T, proto string, pusher TracesPusher, a *modeAuthorizer) {
	t.Helper()
	level := dslog.Level{}
	_ = level.Set("info")
	cfg := map[string]interface{}{"otlp": map[string]interface{}{"protocols": map[string]interface{}{proto: nil}}}
	shim, err := New(cfg, pusher, MultiTenancyMiddleware(), 0, level, prometheus.NewPedanticRegistry(), WithOTLPAuthorizer(a.check))
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), shim))
	t.Cleanup(func() { _ = services.StopAndAwaitTerminated(context.Background(), shim) })
}

func postOTLP(t *testing.T, body []byte, auth, orgID string) int {
	t.Helper()
	var code int
	require.Eventually(t, func() bool {
		req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:4318/v1/traces", bytes.NewReader(body))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Scope-OrgID", orgID)
		resp, err := (&http.Client{Timeout: time.Second}).Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		code = resp.StatusCode
		return true
	}, 5*time.Second, 50*time.Millisecond)
	return code
}

func TestOTLPHTTPReceiverAuthModes(t *testing.T) {
	body, err := ptraceotlp.NewExportRequestFromTraces(testdata.GenerateTraces(1)).MarshalProto()
	require.NoError(t, err)

	for _, tc := range []struct {
		mode        xstenant.EnforceMode
		auth, org   string
		wantStatus  int
		wantTenants []string
	}{
		{xstenant.EnforceOff, "Bearer nope", "t-b", http.StatusOK, []string{"t-b"}},
		{xstenant.EnforceShadow, "Bearer nope", "t-b", http.StatusOK, []string{"t-b"}},
		{xstenant.EnforceOn, "Bearer good", "t-a", http.StatusOK, []string{"t-a"}},
		{xstenant.EnforceOn, "Bearer nope", "t-a", http.StatusUnauthorized, nil},
		{xstenant.EnforceOn, "", "t-a", http.StatusUnauthorized, nil},
	} {
		t.Run(tc.mode.String()+"/"+tc.auth+"/"+tc.org, func(t *testing.T) {
			pusher := &tenantPusher{}
			a := &modeAuthorizer{mode: tc.mode}
			runAuthShim(t, "http", pusher, a)
			require.Equal(t, tc.wantStatus, postOTLP(t, body, tc.auth, tc.org))
			require.Equal(t, tc.wantTenants, pusher.got())
		})
	}
}

func TestOTLPGRPCReceiverAuthModes(t *testing.T) {
	for _, tc := range []struct {
		mode        xstenant.EnforceMode
		auth, org   string
		wantCode    codes.Code
		wantTenants []string
	}{
		{xstenant.EnforceOff, "Bearer nope", "t-b", codes.OK, []string{"t-b"}},
		{xstenant.EnforceShadow, "Bearer nope", "t-b", codes.OK, []string{"t-b"}},
		{xstenant.EnforceOn, "Bearer good", "t-a", codes.OK, []string{"t-a"}},
		{xstenant.EnforceOn, "Bearer nope", "t-a", codes.Unauthenticated, nil},
		{xstenant.EnforceOn, "Bearer good", "t-b", codes.Unauthenticated, nil},
	} {
		t.Run(tc.mode.String()+"/"+tc.auth+"/"+tc.org, func(t *testing.T) {
			pusher := &tenantPusher{}
			a := &modeAuthorizer{mode: tc.mode}
			runAuthShim(t, "grpc", pusher, a)

			conn, err := grpc.NewClient("127.0.0.1:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			defer conn.Close()

			client := ptraceotlp.NewGRPCClient(conn)
			req := ptraceotlp.NewExportRequestFromTraces(testdata.GenerateTraces(1))
			ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", tc.auth, "x-scope-orgid", tc.org)
			require.Eventually(t, func() bool {
				cctx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				_, err = client.Export(cctx, req)
				return status.Code(err) != codes.Unavailable
			}, 5*time.Second, 50*time.Millisecond)
			require.Equal(t, tc.wantCode, status.Code(err))
			require.Equal(t, tc.wantTenants, pusher.got())

			reqs := a.requests()
			require.NotEmpty(t, reqs)
			require.Equal(t, "/opentelemetry.proto.collector.trace.v1.TraceService/Export", reqs[0].Path)
		})
	}
}

// Receivers other than OTLP are not checked.
func TestAuthIsOTLPOnly(t *testing.T) {
	a := &modeAuthorizer{mode: xstenant.EnforceOn}
	level := dslog.Level{}
	_ = level.Set("info")
	cfg := map[string]interface{}{"zipkin": nil}
	shim, err := New(cfg, &tenantPusher{}, FakeTenantMiddleware(), 0, level, prometheus.NewPedanticRegistry(), WithOTLPAuthorizer(a.check))
	require.NoError(t, err)
	require.NotNil(t, shim)
	require.Empty(t, a.requests())
}

func httpCtx(auth, orgID string) context.Context {
	return client.NewContext(context.Background(), client.Info{
		Metadata: client.NewMetadata(map[string][]string{"authorization": {auth}, "x-scope-orgid": {orgID}}),
	})
}
