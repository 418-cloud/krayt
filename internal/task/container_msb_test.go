package task

import "testing"

func TestValidateContainerPolicyForMsb(t *testing.T) {
	if err := ValidateContainerPolicyForMsb(ContainerPolicy{}); err != nil {
		t.Errorf("zero value should be valid, got %v", err)
	}
	cases := []ContainerPolicy{
		{AddCapabilities: []string{"CAP_NET_ADMIN"}},
		{SeccompUnconfined: true},
		{ReadonlyRootfs: true},
	}
	for _, cp := range cases {
		if err := ValidateContainerPolicyForMsb(cp); err == nil {
			t.Errorf("ValidateContainerPolicyForMsb(%+v) = nil, want an error naming --security", cp)
		}
	}
}
