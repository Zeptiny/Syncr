package tools

import (
	"fmt"
	"os"
	"testing"
)

// helperEnv makes the test binary act as a fake `faketool` that prints a
// version line instead of running tests (see TestProcDetector).
const helperEnv = "SYNCR_TOOLS_TEST_HELPER_OUTPUT"

func TestMain(m *testing.M) {
	if out, ok := os.LookupEnv(helperEnv); ok {
		fmt.Println(out)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
