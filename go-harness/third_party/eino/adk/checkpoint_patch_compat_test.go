/*
 * Copyright 2026 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package adk

import (
	"bytes"
	"context"
	"encoding/gob"
	"testing"

	"github.com/cloudwego/eino/internal/core"
)

// These focused compatibility cases accompany the #1232 backport. Application
// tests additionally exercise public Runner size and durable tool reconstruction.
func TestCheckpointPatchReadsLegacyInlineState(t *testing.T) {
	payload := []byte("legacy native state")
	saved := &serialization{
		Info:              &InterruptInfo{Data: &ChatModelAgentInterruptInfo{Data: payload}},
		InterruptID2State: map[string]core.InterruptState{"root": {State: payload}},
	}
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(saved); err != nil {
		t.Fatal(err)
	}
	store := newMyStore()
	store.m["checkpoint"] = buffer.Bytes()
	_, _, info, err := runnerLoadCheckPointImpl(store, context.Background(), "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	state, ok := info.Data.(*ChatModelAgentInterruptInfo)
	if !ok || !bytes.Equal(state.Data, payload) {
		t.Fatal("legacy inline state was not preserved")
	}
}

func TestCheckpointPatchRejectsBrokenReferences(t *testing.T) {
	for _, item := range []struct {
		name string
		info *InterruptInfo
		data map[string]core.InterruptState
	}{
		{"missing-info", nil, map[string]core.InterruptState{"root": {State: []byte("state")}}},
		{"wrong-info", &InterruptInfo{Data: "custom"}, map[string]core.InterruptState{"root": {State: []byte("state")}}},
		{"missing-state", &InterruptInfo{Data: &ChatModelAgentInterruptInfo{}}, nil},
		{"wrong-state-type", &InterruptInfo{Data: &ChatModelAgentInterruptInfo{}}, map[string]core.InterruptState{"root": {State: "not bytes"}}},
	} {
		t.Run(item.name, func(t *testing.T) {
			saved := &serialization{Info: item.info, InfoDataSourceInterruptID: "root", InterruptID2State: item.data}
			var buffer bytes.Buffer
			if err := gob.NewEncoder(&buffer).Encode(saved); err != nil {
				t.Fatal(err)
			}
			store := newMyStore()
			store.m["checkpoint"] = buffer.Bytes()
			if _, _, _, err := runnerLoadCheckPointImpl(store, context.Background(), "checkpoint"); err == nil {
				t.Fatal("broken compacted reference silently loaded")
			}
		})
	}
}

func TestCheckpointPatchKeepsDifferentCustomData(t *testing.T) {
	event := StatefulInterrupt(context.Background(), "interrupt", []byte("canonical state"))
	for _, info := range []*InterruptInfo{
		{Data: []byte("custom view")},
		{Data: &ChatModelAgentInterruptInfo{Data: []byte("different native state")}},
	} {
		got, reference := compactRunnerCheckpointInfoData(info, event.Action.internalInterrupted)
		if got != info || reference != "" {
			t.Fatal("different custom interrupt data was compacted")
		}
	}
}
