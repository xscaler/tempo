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

// parseEnforceMode parses the configured push enforcement mode. An empty value is off. validate
// rejects invalid values before a source is built.
func parseEnforceMode(v string) xstenant.EnforceMode {
	if v == "" {
		return xstenant.EnforceOff
	}
	m, err := xstenant.ParseEnforceMode(v)
	if err != nil {
		return xstenant.EnforceOff
	}
	return m
}

func (s *adminStoreSource) checkPush(tenant string) (bool, string) {
	return s.reader.CheckPush(tenant, s.enforce)
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
