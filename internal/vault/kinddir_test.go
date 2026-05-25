package vault

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestKindDir_TaskMapsToTasks pins the operator-facing
// convention: `kind: task` lands on `<root>/tasks/` (plural)
// rather than the literal-kind `<root>/task/`. The split is the
// only override today — every other kind maps verbatim.
func TestKindDir_TaskMapsToTasks(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "tasks", KindDir("task"))
}

// TestKindDir_OtherKindsPassThrough confirms the override is
// scoped to `task` — adding a new override accidentally to the
// helper would surface here.
func TestKindDir_OtherKindsPassThrough(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"person", "boardgame", "github-pr", "day", "github", ""} {
		assert.Equal(t, k, KindDir(k), "non-task kind %q must pass through verbatim", k)
	}
}
