package harness

import "testing"

func TestPermissionModeMatchesLocalACPClassification(t *testing.T) {
	for _, tc := range []struct {
		name                string
		off, dangerous, all bool
	}{
		{"read_file", false, false, true},
		{"mcp/files/view_image", false, false, true},
		{"search_files", false, false, true},
		{"task", false, false, true},
		{"write_todos", false, true, true},
		{"skill", false, true, true},
		{"web_search", false, true, true},
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
