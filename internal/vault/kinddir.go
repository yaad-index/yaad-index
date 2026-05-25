package vault

// KindDir maps a canonical entity kind to the vault subdirectory
// name that hosts its files. For most kinds the directory name
// matches the kind verbatim (`person` → `person/`, `boardgame` →
// `boardgame/`). The `task` kind is the singular outlier: the
// operator-facing convention pins the directory at `tasks/`
// (plural) since it predates the #268 entity promotion, while the
// canonical kind stays singular like every other.
//
// Centralizing the rule here keeps reader / writer / archive /
// destroy paths consistent so set_property targeting `task:X` and
// task_append targeting the same id land on the same on-disk file.
func KindDir(kind string) string {
	if kind == "task" {
		return "tasks"
	}
	return kind
}
