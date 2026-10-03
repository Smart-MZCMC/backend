package feature

import (
	"os"
	"testing"

	"github.com/stretchr/testify/suite"

	"smart-mzcmc/tests"
)

// TestMain 在整包测试跑完后删掉临时数据库目录。
//
// 由 TestMain 兜底而不是每个用例的 TearDownTest：init() 里建的目录属于整包，
// 而用例级的清理在并行运行（t.Parallel）或中途 panic 时都可能不执行。
func TestMain(m *testing.M) {
	code := m.Run()
	tests.Cleanup()
	os.Exit(code)
}

type ExampleTestSuite struct {
	suite.Suite
	tests.TestCase
}

func TestExampleTestSuite(t *testing.T) {
	suite.Run(t, new(ExampleTestSuite))
}

// SetupTest will run before each test in the suite.
func (s *ExampleTestSuite) SetupTest() {
}

// TearDownTest will run after each test in the suite.
func (s *ExampleTestSuite) TearDownTest() {
}

func (s *ExampleTestSuite) TestIndex() {
	s.True(true)
}
