//go:build darwin || linux

package process

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestUnsupportedRenameErrorClassification(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{errors.ErrUnsupported, true},
		{syscall.EOPNOTSUPP, true},
		{fmt.Errorf("wrapped syscall: %w", syscall.ENOSYS), true},
		{&os.LinkError{Op: "exclusive rename", Old: "from", New: "to", Err: syscall.EOPNOTSUPP}, true},
		{syscall.EINVAL, false},
		{syscall.ENOTSUP, true},
		{syscall.EACCES, false},
		{syscall.EEXIST, false},
		{syscall.EXDEV, false},
		{errors.New("unrelated error"), false},
		{nil, false},
	} {
		if got := isUnsupportedRenameError(test.err); got != test.want {
			t.Errorf("isUnsupportedRenameError(%v) = %t, want %t", test.err, got, test.want)
		}
	}
}
