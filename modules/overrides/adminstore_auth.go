package overrides

import "github.com/xscaler/dskit/xstenant"

// RequestAuthorizer is implemented by overrides that can authorize an API key from the keys
// published to the xscaler admin store.
type RequestAuthorizer interface {
	// CheckAuth decides one request under the configured enforce_auth mode.
	CheckAuth(req xstenant.AuthRequest) xstenant.AuthDecision
}

// CheckAuth asks o to authorize the request. It allows everything, without replacing the tenant,
// when o does not read the admin store or enforce_auth is off.
func CheckAuth(o Interface, req xstenant.AuthRequest) xstenant.AuthDecision {
	if a, ok := o.(RequestAuthorizer); ok {
		return a.CheckAuth(req)
	}
	return xstenant.AuthDecision{Allowed: true, Reason: xstenant.ReasonAuthAllowed}
}

func (s *adminStoreSource) checkAuth(req xstenant.AuthRequest) xstenant.AuthDecision {
	return s.reader.CheckAuth(req, s.enforceAuth)
}

// CheckAuth implements RequestAuthorizer.
func (o *runtimeConfigOverridesManager) CheckAuth(req xstenant.AuthRequest) xstenant.AuthDecision {
	if o.adminStore == nil {
		return xstenant.AuthDecision{Allowed: true, Reason: xstenant.ReasonAuthAllowed}
	}
	return o.adminStore.checkAuth(req)
}

// CheckAuth implements RequestAuthorizer.
func (o *userConfigurableOverridesManager) CheckAuth(req xstenant.AuthRequest) xstenant.AuthDecision {
	return CheckAuth(o.Interface, req)
}
