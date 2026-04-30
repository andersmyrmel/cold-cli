package engine

import "testing"

func TestSecretResolverFuncIsExposed(t *testing.T) {
	resolver := SecretResolverFunc(func(ref string) (string, error) {
		return "secret:" + ref, nil
	})

	value, err := resolver.ResolveSecret("abc")
	if err != nil {
		t.Fatalf("ResolveSecret error: %v", err)
	}
	if value != "secret:abc" {
		t.Fatalf("expected secret:abc, got %q", value)
	}
}
