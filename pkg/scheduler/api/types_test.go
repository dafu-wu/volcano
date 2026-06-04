/*
Copyright 2026 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import "testing"

func TestContainsPreemptActionFatalStatus(t *testing.T) {
	tests := []struct {
		name     string
		statuses StatusSets
		want     bool
	}{
		{
			name: "DRA allocation failure is preempt/reclaim resolvable",
			statuses: StatusSets{
				{Code: UnschedulableAndUnresolvable, Plugin: "DynamicResources", Reason: "cannot allocate all claims"},
			},
			want: false,
		},
		{
			name: "DRA failure can be mixed with ordinary unschedulable status",
			statuses: StatusSets{
				{Code: Unschedulable, Plugin: "NodePorts", Reason: "node(s) didn't have free ports for the requested pod ports"},
				{Code: UnschedulableAndUnresolvable, Plugin: "DynamicResources", Reason: "cannot allocate all claims"},
			},
			want: false,
		},
		{
			// Volcano invokes the DRA plugin's Filter() directly (not via the
			// framework's RunFilterPlugins), so the returned status has an empty
			// Plugin name. The DRA "cannot allocate all claims" failure must still
			// be recognized as preempt/reclaim resolvable regardless of plugin name.
			name: "DRA allocation failure with empty plugin name is still resolvable",
			statuses: StatusSets{
				{Code: Unschedulable, Plugin: "", Reason: "node(s) didn't have free ports for the requested pod ports"},
				{Code: UnschedulableAndUnresolvable, Plugin: "", Reason: "cannot allocate all claims"},
			},
			want: false,
		},

		{
			name: "non DRA unresolvable status stays fatal",
			statuses: StatusSets{
				{Code: UnschedulableAndUnresolvable, Plugin: "NodeAffinity", Reason: "node(s) didn't match Pod's node affinity"},
			},
			want: true,
		},
		{
			name: "DRA plus another fatal status stays fatal",
			statuses: StatusSets{
				{Code: UnschedulableAndUnresolvable, Plugin: "DynamicResources", Reason: "cannot allocate all claims"},
				{Code: Error, Plugin: "VolumeBinding", Reason: "unexpected error"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.statuses.ContainsPreemptActionFatalStatus(); got != tt.want {
				t.Fatalf("ContainsPreemptActionFatalStatus() = %v, want %v", got, tt.want)
			}
		})
	}
}
