package tests

import (
	"github.com/goravel/framework/testing"

	"smart-mzcmc/bootstrap"
)

func init() {
	bootstrap.Boot()
}

type TestCase struct {
	testing.TestCase
}
