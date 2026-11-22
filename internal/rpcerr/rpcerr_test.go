package rpcerr

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
)

func TestIs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		err            error
		stale, revoked bool
	}{
		{name: "stale", err: StaleEpoch(), stale: true},
		{name: "revoked", err: RunnerRevoked(), revoked: true},
		{name: "wrapped", err: fmt.Errorf("提交结果：%w", StaleEpoch()), stale: true},
		{name: "wrong code", err: connect.NewError(connect.CodeInternal, errors.New(ReasonStaleEpoch))},
		{name: "other", err: connect.NewError(connect.CodePermissionDenied, errors.New("denied"))},
		{name: "plain", err: errors.New(ReasonRunnerRevoked)},
		{name: "nil"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := IsStaleEpoch(c.err); got != c.stale {
				t.Errorf("IsStaleEpoch = %v，期望 %v", got, c.stale)
			}
			if got := IsRunnerRevoked(c.err); got != c.revoked {
				t.Errorf("IsRunnerRevoked = %v，期望 %v", got, c.revoked)
			}
		})
	}
}
