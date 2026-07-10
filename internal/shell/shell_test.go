package shell

import "testing"

func TestIsSupported(t *testing.T) {
	for _, name := range Supported {
		if !IsSupported(name) {
			t.Errorf("%q should be supported", name)
		}
	}
	if IsSupported("powershell") {
		t.Error("powershell should not be supported")
	}
}
