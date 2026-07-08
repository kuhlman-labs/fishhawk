package identity

import "context"

// NoOpIdentityProvider is the deny-by-default provider selected when
// no OAuth client config is present. Every method returns a
// least-privilege safe value so an unconfigured backend can never
// grant access through the identity surface.
type NoOpIdentityProvider struct{}

// Compile-time assertion that NoOpIdentityProvider satisfies the
// interface.
var _ IdentityProvider = (*NoOpIdentityProvider)(nil)

// NewNoOp returns the deny-by-default identity provider.
func NewNoOp() IdentityProvider { return &NoOpIdentityProvider{} }

// VerifyUser always fails closed: there is no forge to authorize
// against, so it returns ErrNotConfigured with an empty subject.
func (*NoOpIdentityProvider) VerifyUser(context.Context, DeviceCodePrompt) (string, error) {
	return "", ErrNotConfigured
}

// PermissionLevel always returns PermissionNone (nil error): an
// unconfigured provider grants no permission to anyone.
func (*NoOpIdentityProvider) PermissionLevel(context.Context, string, string) (Permission, error) {
	return PermissionNone, nil
}

// ResolveMembership always returns false (nil error): an unconfigured
// provider treats every subject as a non-member.
func (*NoOpIdentityProvider) ResolveMembership(context.Context, string, string) (bool, error) {
	return false, nil
}
