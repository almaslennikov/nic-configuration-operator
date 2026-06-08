/*
2026 NVIDIA CORPORATION & AFFILIATES
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

package spectrumx

import (
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/Mellanox/nic-configuration-operator/pkg/types"
)

// prepare-stage semantic group names.
const (
	groupBreakout             = "breakout"
	groupPostBreakoutNVConfig = "post-breakout-nvconfig"
)

// GetPrepareOps renders + parses the prepare-stage plan and returns its breakout
// (group "breakout") and post-breakout (group "post-breakout-nvconfig") ops, then
// applies rawNvConfig overrides. The ConfigurationManager owns checking/applying these
// so they sequence with other NVConfig options.
func (m *spectrumXConfigManager) GetPrepareOps(device *v1alpha1.NicDevice) (breakout, postBreakout []types.DMSConfigOp, err error) {
	data, err := m.generatePlan(device, StagePrepare)
	if err != nil {
		return nil, nil, err
	}
	plan, err := ParsePlan(data)
	if err != nil {
		return nil, nil, err
	}

	if g, ok := plan.Group(groupBreakout); ok {
		if breakout, err = g.Ops(plan.Params); err != nil {
			return nil, nil, err
		}
	}
	if g, ok := plan.Group(groupPostBreakoutNVConfig); ok {
		if postBreakout, err = g.Ops(plan.Params); err != nil {
			return nil, nil, err
		}
	}

	breakout, postBreakout = applyRawNvConfigOverrides(device, breakout, postBreakout)
	log.Log.V(2).Info("parsed prepare-stage knobs", "device", device.Name, "breakoutOps", len(breakout), "postBreakoutOps", len(postBreakout))
	return breakout, postBreakout, nil
}

// applyRawNvConfigOverrides merges template.rawNvConfig onto the parsed prepare ops.
// A raw entry whose Name matches a "<path>/<leaf>" of an existing op overrides that
// leaf's value (in whichever phase it lives); otherwise it is appended to the
// post-breakout phase as its own op.
//
// Open item O11: in the plan flow rawNvConfig names are interpreted as DMS leaf paths
// (<path>/<leaf>), not mlxconfig keys as in the legacy flow.
func applyRawNvConfigOverrides(device *v1alpha1.NicDevice, breakout, postBreakout []types.DMSConfigOp) ([]types.DMSConfigOp, []types.DMSConfigOp) {
	tmpl := device.Spec.Configuration.Template
	if tmpl == nil || len(tmpl.RawNvConfig) == 0 {
		return breakout, postBreakout
	}

	for _, raw := range tmpl.RawNvConfig {
		path, leaf := splitLeafPath(raw.Name)
		if overrideLeaf(breakout, path, leaf, raw.Value) || overrideLeaf(postBreakout, path, leaf, raw.Value) {
			continue
		}
		postBreakout = append(postBreakout, types.DMSConfigOp{Path: path, Values: map[string]any{leaf: raw.Value}})
	}
	return breakout, postBreakout
}

// overrideLeaf sets the value of an existing op's leaf, returning true if found.
func overrideLeaf(ops []types.DMSConfigOp, path, leaf, value string) bool {
	for i := range ops {
		if ops[i].Path == path {
			if _, ok := ops[i].Values[leaf]; ok {
				ops[i].Values[leaf] = value
				return true
			}
		}
	}
	return false
}

// splitLeafPath splits "<path>/<leaf>" into its container path and leaf name.
func splitLeafPath(full string) (path, leaf string) {
	idx := strings.LastIndex(full, "/")
	if idx <= 0 {
		return full, full
	}
	return full[:idx], full[idx+1:]
}
