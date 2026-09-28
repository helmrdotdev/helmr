package firecracker

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	sdkInitTimeoutEnvironment    = "FIRECRACKER_GO_SDK_INIT_TIMEOUT_SECONDS"
	sdkRequestTimeoutEnvironment = "FIRECRACKER_GO_SDK_REQUEST_TIMEOUT_MILLISECONDS"
)

var sdkClientEnvironmentMu sync.Mutex

// The SDK reads its initialization and request timeouts from process environment while
// constructing a client. Keep that implementation detail inside this adapter
// and serialize the short construction window so concurrent VM starts cannot
// observe one another's timeout.
func withSDKClientTimeouts(timeout time.Duration, construct func() error) (err error) {
	sdkClientEnvironmentMu.Lock()
	defer sdkClientEnvironmentMu.Unlock()

	seconds := timeout / time.Second
	if timeout%time.Second != 0 {
		seconds++
	}
	previous, existed := os.LookupEnv(sdkInitTimeoutEnvironment)
	if err := os.Setenv(sdkInitTimeoutEnvironment, strconv.FormatInt(int64(seconds), 10)); err != nil {
		return err
	}
	defer func() {
		var restoreErr error
		if existed {
			restoreErr = os.Setenv(sdkInitTimeoutEnvironment, previous)
		} else {
			restoreErr = os.Unsetenv(sdkInitTimeoutEnvironment)
		}
		err = errors.Join(err, restoreErr)
	}()
	previousRequest, requestExisted := os.LookupEnv(sdkRequestTimeoutEnvironment)
	milliseconds := timeout / time.Millisecond
	if timeout%time.Millisecond != 0 {
		milliseconds++
	}
	if err := os.Setenv(sdkRequestTimeoutEnvironment, strconv.FormatInt(int64(milliseconds), 10)); err != nil {
		return err
	}
	defer func() {
		var restoreErr error
		if requestExisted {
			restoreErr = os.Setenv(sdkRequestTimeoutEnvironment, previousRequest)
		} else {
			restoreErr = os.Unsetenv(sdkRequestTimeoutEnvironment)
		}
		err = errors.Join(err, restoreErr)
	}()
	return construct()
}
