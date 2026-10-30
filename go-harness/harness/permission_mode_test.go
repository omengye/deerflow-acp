package harness

import "testing"

func TestPermissionModeToolClassification(t *testing.T) {
	for _, tc := range []struct {
		name                string
		off, dangerous, all bool
	}{
		{"read_file", false, false, true},
		{"mcp/files/view_image", false, true, true},
		{"mcp_reader_delete_record_1234", false, true, true},
		{"read_then_delete", false, true, true},
		{"mcp_task_search_write_record_1234", false, true, true},
		{"Read_File", false, true, true},
		{"search_files", false, false, true},
		{"task", false, false, true},
		{"write_todos", false, true, true},
		{"skill", false, false, true},
		{"skill_inspect", false, false, true},
		{"list_directory", false, false, true},
		{"read_skill_file", false, false, true},
		{"skill_edit", false, true, true},
		{"skill_manage", false, true, true},
		{"mcp/custom/skill", false, true, true},
		{"mcp/custom/skill_inspect", false, true, true},
		{"mcp/custom/list_directory", false, true, true},
		{"skill_execute", false, true, true},
		{"web_search", false, true, true},
		{"bash", false, true, true},
		{"execute_command", false, true, true},
	} {
		for _, mode := range []struct {
			value PermissionMode
			want  bool
		}{{PermissionModeOff, tc.off}, {PermissionModeDangerous, tc.dangerous}, {PermissionModeAll, tc.all}} {
			if got := mode.value.RequiresPermission(tc.name); got != mode.want {
				t.Errorf("mode=%s tool=%s got=%t want=%t", mode.value, tc.name, got, mode.want)
			}
		}
	}
	if err := (PermissionMode("invalid")).Validate(); err == nil {
		t.Fatal("accepted an unknown permission mode")
	}
}

func TestUnknownToolCannotImpersonateBuiltin(t *testing.T) {
	for _, name := range []string{"read_file", "task", "skill", "view_image", "search_memory"} {
		if !PermissionModeDangerous.RequiresToolPermission(name, false) {
			t.Fatalf("untrusted %q bypassed approval", name)
		}
		if PermissionModeDangerous.RequiresToolPermission(name, true) {
			t.Fatalf("trusted built-in %q unexpectedly needs approval", name)
		}
		if PermissionModeOff.RequiresToolPermission(name, false) {
			t.Fatalf("explicit host off policy was changed for %q", name)
		}
	}
}
