package image

import (
	"github.com/kubecd/kubecd/pkg/exec"
	"testing"
)

// TestHelperProcess is required boilerplate (one per package) for using exec.TestRunner
func TestHelperProcess(t *testing.T) {
	exec.InsideHelperProcess()
}
