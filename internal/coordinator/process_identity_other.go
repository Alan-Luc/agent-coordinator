//go:build !darwin

package coordinator

import "errors"

var errProcessIdentityUnsupported = errors.New("process identity is unsupported on this operating system")

func currentBoot() (string, error) { return "", errProcessIdentityUnsupported }
func identifyProcess(int) (ProcessIdentity, error) {
	return ProcessIdentity{}, errProcessIdentityUnsupported
}
func compareProcess(ProcessIdentity) (ProcessStatus, error) {
	return ProcessUnknown, errProcessIdentityUnsupported
}
func processGroup(int) (int, error)           { return 0, errProcessIdentityUnsupported }
func foregroundProcessGroup(int) (int, error) { return 0, errProcessIdentityUnsupported }
