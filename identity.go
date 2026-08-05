package agent

// TenantKey is an opaque tenant identity shared by Agent SDK packages.
// Portable SDK code may compare and persist it, but must not interpret its value.
type TenantKey string

// Valid reports whether the key can scope a persisted SDK operation.
func (k TenantKey) Valid() bool {
	return k != ""
}
