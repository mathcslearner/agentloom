package version

import "testing"

func TestVersionIsSet(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must never be empty; default is \"dev\"")
	}
}
