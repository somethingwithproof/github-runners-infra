package compute

import (
	"strings"
	"testing"
)

func TestValidControllerID(t *testing.T) {
	for _, value := range []string{"controller-1", "A", "runner_controller"} {
		if !ValidControllerID(value) {
			t.Fatalf("ValidControllerID(%q) = false", value)
		}
	}
	for _, value := range []string{"", "-controller", "_controller", "controller space", strings.Repeat("a", 65)} {
		if ValidControllerID(value) {
			t.Fatalf("ValidControllerID(%q) = true", value)
		}
	}
}
