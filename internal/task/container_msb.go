package task

import "fmt"

// ValidateContainerPolicyForMsb is the msb-era pre-flight check for the `container:` config
// block (run-tasks-on-microsandbox.md decision 5). msb offers only `--security default|restricted`
// — nothing finer — so the three OCI-spec knobs harden-container-oci-spec.md built have no msb
// equivalent. Each is a REMOVED key that hard-errors, naming `--security` as the replacement,
// exactly the reasoning already applied to `network.mitm`: a config that sets one of these is a
// config reasoning about hardening, and silently dropping it would be a posture regression a
// human would not notice until it mattered. A zero-value ContainerPolicy — the common case, no
// container: block at all — passes with no error.
func ValidateContainerPolicyForMsb(cp ContainerPolicy) error {
	switch {
	case len(cp.AddCapabilities) > 0:
		return fmt.Errorf("container.capabilities is not a valid key under msb — msb offers only " +
			"--security default|restricted, with no finer-grained Linux capability control; remove " +
			"container.capabilities")
	case cp.SeccompUnconfined:
		return fmt.Errorf("container.seccomp: unconfined is not a valid key under msb — msb has no " +
			"seccomp profile knob, only --security default|restricted; remove container.seccomp")
	case cp.ReadonlyRootfs:
		return fmt.Errorf("container.readonly_rootfs is not a valid key under msb — msb has no " +
			"read-only-rootfs option, only --security default|restricted; remove container.readonly_rootfs")
	default:
		return nil
	}
}
