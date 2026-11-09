package overrides

import "github.com/xscaler/dskit/xstenant"

// PushGate is implemented by overrides that can deny a tenant's pushes from the tenant state
// published to the xscaler admin store.
type PushGate interface {
	// CheckPush reports whether pushes from the tenant are denied, and why.
	CheckPush(tenant string) (deny bool, reason string)
}

// CheckPush asks o whether the tenant's pushes are denied. It never denies when o does not read
// the admin store or enforce_state is off. Only a soft locked tenant is ever denied.
func CheckPush(o Interface, tenant string) (deny bool, reason string) {
	if g, ok := o.(PushGate); ok {
		return g.CheckPush(tenant)
	}
	return false, ""
}

// enforceMode returns the configured push enforcement mode. validate has checked the value.
func (c *AdminStoreConfig) enforceMode() xstenant.EnforceMode {
	m, err := xstenant.ParseEnforceMode(c.EnforceState)
	if err != nil { // empty or invalid: validate rejects invalid values
		return xstenant.EnforceOff
	}
	return m
}

func (s *adminStoreSource) checkPush(tenant string) (bool, string) {
	return s.reader.CheckPush(tenant, s.cfg.enforceMode())
}

// CheckPush implements PushGate.
func (o *runtimeConfigOverridesManager) CheckPush(tenant string) (bool, string) {
	if o.adminStore == nil {
		return false, ""
	}
	return o.adminStore.checkPush(tenant)
}

// CheckPush implements PushGate.
func (o *userConfigurableOverridesManager) CheckPush(tenant string) (bool, string) {
	return CheckPush(o.Interface, tenant)
}
