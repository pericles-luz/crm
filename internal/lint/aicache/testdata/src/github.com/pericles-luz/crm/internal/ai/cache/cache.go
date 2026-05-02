// Package cache is a stub of internal/ai/cache used by the aicache analyzer
// testdata fixtures.
package cache

type Key struct {
	value string
}

func (k Key) String() string { return k.value }
func (k Key) IsZero() bool   { return k.value == "" }

func TenantKey(tenantID, conv, untilMsg string) (Key, error) {
	return Key{value: "tenant:" + tenantID + ":ai:summary:" + conv + ":" + untilMsg}, nil
}

func SystemKey(scope, conv, untilMsg string) (Key, error) {
	return Key{value: "system:ai:" + scope + ":" + conv + ":" + untilMsg}, nil
}
