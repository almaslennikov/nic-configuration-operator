/*
2025 NVIDIA CORPORATION & AFFILIATES
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
	"sync"
	"sync/atomic"
	"time"

	execUtils "k8s.io/utils/exec"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
	"github.com/Mellanox/nic-configuration-operator/pkg/dms"
	"github.com/Mellanox/nic-configuration-operator/pkg/types"
)

type SpectrumXManager interface {
	// GetPrepareOps renders the device's prepare-stage plan and returns its DMS ops
	// divided into the breakout (group "breakout") and post-breakout (group
	// "post-breakout-nvconfig") phases, with rawNvConfig overrides applied. The
	// ConfigurationManager owns checking/applying them (so they sequence with other
	// NVConfig options); the breakout phase requires a reboot before post-breakout.
	GetPrepareOps(device *v1alpha1.NicDevice) (breakout, postBreakout []types.DMSConfigOp, err error)
	// RuntimeConfigApplied checks if the desired Spectrum-X runtime spec is applied to the device
	RuntimeConfigApplied(device *v1alpha1.NicDevice) (bool, error)
	// ApplyRuntimeConfig applies the desired Spectrum-X runtime spec to the device
	ApplyRuntimeConfig(device *v1alpha1.NicDevice) (*types.RuntimeConfigurationApplyResult, error)
	// GetDocaCCTargetVersion returns the target version of DOCA SPC-X CC for the device
	GetDocaCCTargetVersion(device *v1alpha1.NicDevice) (string, error)
	// RunDocaSpcXCC launches and keeps track of the DOCA SPC-X CC process for the given port
	RunDocaSpcXCC(port v1alpha1.NicDevicePortSpec) error
	// GetCCTerminationChannel returns a read-only channel that receives the RDMA interface name
	// when a DOCA SPC-X CC process terminates unexpectedly after startup
	GetCCTerminationChannel() <-chan string
}

type spectrumXConfigManager struct {
	dmsManager    dms.DMSManager
	execInterface execUtils.Interface
	// blueprintsBaseDir is the on-disk base directory under which the daemon
	// materializes blueprint ConfigMaps (one subdir per ConfigMap name); the planner
	// consumes <base>/<version> via --blueprints-root. Always non-empty (defaulted in
	// the constructor); library consumers override it. See docs/design-dms-blueprints-spcx.md §3.3.
	blueprintsBaseDir string
	// planner renders the do-SPCX plan. Currently a stub returning the committed
	// example plans until DMS ships the /nvidia/blueprints/plan action.
	planner Planner

	ccProcesses       map[string]*ccProcess
	ccTerminationChan chan string // buffered; carries RDMA iface name on unexpected exit
}

type ccProcess struct {
	port v1alpha1.NicDevicePortSpec
	cmd  execUtils.Cmd

	running            atomic.Bool
	startupCheckPassed atomic.Bool // set after the 3s startup window; distinguishes startup failures from runtime crashes

	// Error handling with mutex protection
	errMutex sync.RWMutex
	cmdErr   error
}

// GetDocaCCTargetVersion returns the target version of DOCA SPC-X CC to install for
// the device, or "" to install nothing (use whatever is already present).
//
// Open item O12: the do-SPCX plan does not carry a doca_spcx_cc binary version, so we
// cannot derive a target version from it yet. We return "" — the controller then skips
// the version-specific install while ApplyRuntimeConfig still launches the binary before
// the cc group. See docs/design-dms-blueprints-spcx.md.
func (m *spectrumXConfigManager) GetDocaCCTargetVersion(device *v1alpha1.NicDevice) (string, error) {
	if spectrumXSpec(device) == nil {
		log.Log.V(2).Info("SpectrumXConfigManager.GetDocaCCTargetVersion(): device SPC-X spec is empty, no DOCA SPC-X CC required", "device", device.Name)
		return "", nil
	}
	return "", nil
}

func (m *spectrumXConfigManager) IsDocaSpcXCCRunning(rdmaInterface string) bool {
	runningCCProcess, found := m.ccProcesses[rdmaInterface]
	if found && runningCCProcess.running.Load() {
		return true
	}
	return false
}

// RunDocaSpcXCC launches and keeps track of the DOCA SPC-X CC process for the given port
func (m *spectrumXConfigManager) RunDocaSpcXCC(port v1alpha1.NicDevicePortSpec) error {
	log.Log.Info("SpectrumXConfigManager.RunDocaSpcXCC()", "rdma", port.RdmaInterface)

	// use rdma interface name as key for ccProcesses map
	// because with HW PLB different ports of the same NIC share the same RDMA device
	if m.IsDocaSpcXCCRunning(port.RdmaInterface) {
		log.Log.V(2).Info("SpectrumXConfigManager.RunDocaSpcXCC(): CC process already running", "rdma", port.RdmaInterface)
		return nil
	}

	cmd := m.execInterface.Command("/opt/mellanox/doca/tools/doca_spcx_cc", "--device", port.RdmaInterface)

	process := &ccProcess{
		port: port,
		cmd:  cmd,
	}

	process.running.Store(true)

	go func() {
		output, err := process.cmd.CombinedOutput()
		if err != nil {
			process.errMutex.Lock()
			process.cmdErr = err
			process.errMutex.Unlock()
			log.Log.Error(err, "SpectrumXConfigManager.RunDocaSpcXCC(): Failed to run CC process", "rdma", port.RdmaInterface)
		}

		log.Log.V(2).Info("SpectrumXConfigManager.RunDocaSpcXCC(): CC process output", "rdma", port.RdmaInterface, "output", string(output))
		process.running.Store(false)

		// Notify controller only for runtime crashes (after startup check passed)
		if process.startupCheckPassed.Load() {
			log.Log.Info("SpectrumXConfigManager.RunDocaSpcXCC(): CC process terminated unexpectedly, sending notification", "rdma", port.RdmaInterface)
			select {
			case m.ccTerminationChan <- port.RdmaInterface:
			default:
				log.Log.V(2).Info("SpectrumXConfigManager.RunDocaSpcXCC(): termination channel full, notification dropped", "rdma", port.RdmaInterface)
			}
		}
	}()

	log.Log.V(2).Info("Waiting 3s for DOCA SPC-X CC to start", "rdma", port.RdmaInterface)
	time.Sleep(3 * time.Second)

	if !process.running.Load() {
		process.errMutex.RLock()
		cmdErr := process.cmdErr
		process.errMutex.RUnlock()

		if cmdErr != nil {
			log.Log.Error(cmdErr, "Failed to start DOCA SPC-X CC", "rdma", port.RdmaInterface)
			return fmt.Errorf("failed to start DOCA SPC-X CC for port %s: %v", port.PCI, cmdErr)
		}
		return fmt.Errorf("failed to start DOCA SPC-X CC for port %s: unknown error", port.RdmaInterface)
	}

	log.Log.V(2).Info("DOCA SPC-X CC process started", "rdma", port.RdmaInterface)

	process.startupCheckPassed.Store(true)
	m.ccProcesses[port.RdmaInterface] = process

	log.Log.Info("Started DOCA SPC-X CC process", "rdma", port.RdmaInterface)

	return nil
}

// GetCCTerminationChannel returns a read-only channel for CC process termination notifications.
// The channel carries the RDMA interface name of the terminated CC process.
func (m *spectrumXConfigManager) GetCCTerminationChannel() <-chan string {
	return m.ccTerminationChan
}

// NewSpectrumXConfigManager creates a SpectrumXManager. blueprintsBaseDir is the
// on-disk base directory under which blueprint ConfigMaps are materialized (one subdir
// per ConfigMap name); pass "" to use the operator in-container default
// (consts.SpectrumXBlueprintsBaseDir). Library consumers pass their own directory.
// See docs/design-dms-blueprints-spcx.md §3.3.
func NewSpectrumXConfigManager(dmsManager dms.DMSManager, blueprintsBaseDir string) SpectrumXManager {
	if blueprintsBaseDir == "" {
		blueprintsBaseDir = consts.SpectrumXBlueprintsBaseDir
	}
	return &spectrumXConfigManager{
		dmsManager:        dmsManager,
		blueprintsBaseDir: blueprintsBaseDir,
		planner:           newStubPlanner(),
		execInterface:     execUtils.New(),
		ccProcesses:       make(map[string]*ccProcess),
		ccTerminationChan: make(chan string, 10),
	}
}
