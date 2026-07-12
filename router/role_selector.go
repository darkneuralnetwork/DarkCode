package router

// RoleSelector decides which roles to activate for a given task type.
type RoleSelector struct{}

func NewRoleSelector() *RoleSelector {
	return &RoleSelector{}
}

// SelectRoles returns the roles that should participate in consensus for this task type.
func (rs *RoleSelector) SelectRoles(task TaskType) []string {
	switch task {
	case TaskTypeSelective:
		return []string{"critic", "verifier"}
	case TaskTypeFullConsensus:
		// all roles
		return []string{"critic", "verifier", "analyst", "creative", "skeptic", "knowledge_booster"}
	default:
		return []string{}
	}
}
