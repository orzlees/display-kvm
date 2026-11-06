//go:build !windows

package windows

import (
	"context"
	"testing"

	"github.com/orzlee/displayKvm/internal/model"
)

func TestNativeUnavailableOutsideWindows(t *testing.T) {
	_, err := NewNative().Enumerate(context.Background())
	requireCode(t, err, model.CodeBackendUnavailable)
}
