package mysql

import (
	"context"
	"strings"
	"testing"

	"github.com/girimi/unredo/internal/executor"
	"github.com/girimi/unredo/internal/ports"
)

func TestApplyRejectsMissingOrOversizeToolVersion(t *testing.T) {
	writer := &ApplyWriter{}
	for _, version := range []string{"", strings.Repeat("v", 33)} {
		_, err := writer.Apply(context.Background(), &ports.Plan{ToolVersion: version}, executor.ApplyOptions{})
		if err == nil || !strings.Contains(err.Error(), "tool_version") {
			t.Fatalf("version %q: err=%v, want tool_version validation error", version, err)
		}
	}
}
