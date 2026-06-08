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
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
	"github.com/Mellanox/nic-configuration-operator/pkg/dms"
	"github.com/Mellanox/nic-configuration-operator/pkg/types"
)

// configure-stage semantic group names consumed by the runtime flow.
const (
	groupCC = "cc"
)

// isHwplb reports whether the device is configured in hardware-PLB multiplane mode.
func isHwplb(device *v1alpha1.NicDevice) bool {
	spec := spectrumXSpec(device)
	return spec != nil && spec.MultiplaneMode == consts.MultiplaneModeHwplb
}

// configurePlan renders + parses the configure-stage plan for the device.
func (m *spectrumXConfigManager) configurePlan(device *v1alpha1.NicDevice) (*Plan, error) {
	data, err := m.generatePlan(device, StageConfigure)
	if err != nil {
		return nil, err
	}
	return ParsePlan(data)
}

// ccRunning reports whether the doca_spcx_cc process is running for the device. In hwplb
// mode all ports share one RDMA device, so only the first port is checked.
func (m *spectrumXConfigManager) ccRunning(device *v1alpha1.NicDevice) bool {
	if len(device.Status.Ports) == 0 {
		return false
	}
	if isHwplb(device) {
		return m.IsDocaSpcXCCRunning(device.Status.Ports[0].RdmaInterface)
	}
	for _, port := range device.Status.Ports {
		if !m.IsDocaSpcXCCRunning(port.RdmaInterface) {
			return false
		}
	}
	return true
}

// startCC launches doca_spcx_cc for the device (waiting for the startup window inside
// RunDocaSpcXCC). In hwplb mode the binary runs once on the first port's RDMA device.
func (m *spectrumXConfigManager) startCC(device *v1alpha1.NicDevice) error {
	if len(device.Status.Ports) == 0 {
		return fmt.Errorf("no ports available for device %s", device.Name)
	}
	if isHwplb(device) {
		log.Log.V(2).Info("startCC(): launching doca_spcx_cc on first port (hwplb)", "device", device.Name, "rdma", device.Status.Ports[0].RdmaInterface)
		return m.RunDocaSpcXCC(device.Status.Ports[0])
	}
	for _, port := range device.Status.Ports {
		log.Log.V(2).Info("startCC(): launching doca_spcx_cc", "device", device.Name, "rdma", port.RdmaInterface)
		if err := m.RunDocaSpcXCC(port); err != nil {
			return err
		}
	}
	return nil
}

// RuntimeConfigApplied reports whether the device's configure-stage knobs are applied.
// It walks the plan's semantic groups in order; before the cc group it also requires the
// doca_spcx_cc process to be running.
func (m *spectrumXConfigManager) RuntimeConfigApplied(device *v1alpha1.NicDevice) (bool, error) {
	log.Log.Info("SpectrumXConfigManager.RuntimeConfigApplied()", "device", device.Name)

	plan, err := m.configurePlan(device)
	if err != nil {
		return false, err
	}

	dmsClient, err := dms.GetDMSClientForDevice(m.dmsManager, device)
	if err != nil {
		log.Log.Error(err, "RuntimeConfigApplied(): failed to get DMS client", "device", device.Name)
		return false, err
	}

	for _, group := range plan.OrderedGroups() {
		if group.Name == groupCC && !m.ccRunning(device) {
			log.Log.Info("RuntimeConfigApplied(): DOCA SPC-X CC is not running", "device", device.Name)
			return false, nil
		}

		ops, err := group.Ops(plan.Params)
		if err != nil {
			return false, err
		}
		log.Log.V(2).Info("RuntimeConfigApplied(): checking group", "device", device.Name, "group", group.Name, "order", group.Order, "ops", len(ops))
		applied, err := dms.OpsApplied(dmsClient, ops)
		if err != nil {
			return false, err
		}
		if !applied {
			log.Log.V(2).Info("RuntimeConfigApplied(): group not applied", "device", device.Name, "group", group.Name)
			return false, nil
		}
	}
	return true, nil
}

// ApplyRuntimeConfig applies the device's configure-stage knobs via DMS, group by group
// in ascending order. Before the cc group it launches doca_spcx_cc and waits for the
// startup window (as today), then applies the cc knobs.
func (m *spectrumXConfigManager) ApplyRuntimeConfig(device *v1alpha1.NicDevice) (*types.RuntimeConfigurationApplyResult, error) {
	log.Log.Info("SpectrumXConfigManager.ApplyRuntimeConfig()", "device", device.Name)

	plan, err := m.configurePlan(device)
	if err != nil {
		return &types.RuntimeConfigurationApplyResult{Status: types.ApplyStatusFailed}, err
	}

	dmsClient, err := dms.GetDMSClientForDevice(m.dmsManager, device)
	if err != nil {
		log.Log.Error(err, "ApplyRuntimeConfig(): failed to get DMS client", "device", device.Name)
		return &types.RuntimeConfigurationApplyResult{Status: types.ApplyStatusFailed}, err
	}

	for _, group := range plan.OrderedGroups() {
		// Start the CC binary (and wait for its startup window) before applying the cc knobs.
		if group.Name == groupCC {
			log.Log.V(2).Info("ApplyRuntimeConfig(): starting DOCA SPC-X CC before cc group", "device", device.Name)
			if err := m.startCC(device); err != nil {
				log.Log.Error(err, "ApplyRuntimeConfig(): failed to start DOCA SPC-X CC", "device", device.Name)
				return &types.RuntimeConfigurationApplyResult{Status: types.ApplyStatusFailed}, err
			}
		}

		ops, err := group.Ops(plan.Params)
		if err != nil {
			return &types.RuntimeConfigurationApplyResult{Status: types.ApplyStatusFailed}, err
		}
		if len(ops) == 0 {
			continue
		}

		log.Log.V(2).Info("ApplyRuntimeConfig(): applying group", "device", device.Name, "group", group.Name, "order", group.Order, "ops", len(ops))
		if err := dmsClient.SetParameters(ops); err != nil {
			log.Log.Error(err, "ApplyRuntimeConfig(): failed to apply group", "device", device.Name, "group", group.Name)
			return &types.RuntimeConfigurationApplyResult{Status: types.ApplyStatusFailed}, err
		}
	}

	return &types.RuntimeConfigurationApplyResult{Status: types.ApplyStatusSuccess}, nil
}
