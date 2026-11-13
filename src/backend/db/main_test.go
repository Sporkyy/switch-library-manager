package db

import (
	"os"
	"testing"

	"github.com/trembon/switch-library-manager/internal/testhome"
)

func TestMain(m *testing.M) {
	os.Exit(testhome.Run(m))
}
