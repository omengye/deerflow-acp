package localhost

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

func TestManagementInstanceBindingRejectsOtherHostBeforeMutation(t *testing.T) {
	var calls atomic.Int32
	manage := func(_ context.Context, r ManagementRequest) (any, error) {
		calls.Add(1)
		if _, exists := r.Fields["expected_instance_id"]; exists {
			t.Error("transport identity leaked into application fields")
		}
		return map[string]bool{"changed": true}, nil
	}
	dir1, dir2 := t.TempDir(), t.TempDir()
	_, a := startHost(t, Config{RuntimeDir: dir1, DataDir: "profile-one", Manage: manage})
	_, b := startHost(t, Config{RuntimeDir: dir2, DataDir: "profile-two", Manage: manage})
	if a.InstanceID == "" || a.InstanceID == a.Token || a.InstanceID == b.InstanceID {
		t.Fatal("invalid instance identity")
	}
	request := json.RawMessage(fmt.Sprintf(`{"operation":"skill.set_enabled","expected_instance_id":%q}`, a.InstanceID))
	if _, err := ManagementCall(context.Background(), dir2, request); err == nil || calls.Load() != 0 {
		t.Fatal("wrong host received a mutation", err)
	}
	// The server also checks bindings for clients that bypass ManagementCall.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", b.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "DFACP/1 %s MANAGE\n", b.Token)
	reader := bufio.NewReader(conn)
	if line, _ := reader.ReadString('\n'); line != "OK\n" {
		t.Fatal(line)
	}
	fmt.Fprintf(conn, "%s\n", request)
	line, _ := reader.ReadString('\n')
	if !strings.Contains(line, "instance_mismatch") || calls.Load() != 0 {
		t.Fatal(line)
	}
	response, err := ManagementCall(context.Background(), dir1, request)
	if err != nil || calls.Load() != 1 || !strings.Contains(string(response), "profile-one") || strings.Contains(string(response), a.Token) {
		t.Fatal(string(response), err)
	}
}
