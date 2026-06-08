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

package dms

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	execUtils "k8s.io/utils/exec"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
	"github.com/Mellanox/nic-configuration-operator/pkg/types"
	"github.com/Mellanox/nic-configuration-operator/pkg/utils"
)

const (
	// dmsClientPath is the new T1/T2 DMS client. It replaces the old `dmsc` client
	// (see dms/t1_t2/arch/transition/SPCX_TRANSITION.md §4: dmsc -> dms-cli/libdms).
	dmsClientPath    = "/opt/mellanox/doca/services/dms/dms-cli"
	dmsClientTimeout = "300s"

	// Flat, target-based T1 QoS / RoCE paths (replacing the old interface-scoped
	// /interfaces/interface[name=…]/nvidia/… paths). See SPCX_TRANSITION.md §2.
	qosPath          = "/nvidia/qos"
	qosTrustModeLeaf = "trust-mode"
	qosPFCPath       = "/nvidia/qos/pfc"
	qosPFCLeaf       = "enabled-priorities"
	roceToSPath      = "/nvidia/roce/tos"
	roceToSLeaf      = "value"
)

// DMSClient interface defines methods for interacting with a DMS server to manage NIC device configuration
type DMSClient interface {
	// GetQoSSettings returns the current QoS settings (trust mode and PFC configuration)
	GetQoSSettings(interfaceName string) (*v1alpha1.QosSpec, error)
	// SetQoSSettings sets the QoS settings for the device (trust mode and PFC configuration)
	SetQoSSettings(spec *v1alpha1.QosSpec) error
	// GetParameters returns the current values of the given operations' leaves,
	// keyed by "<path>/<leaf>".
	GetParameters(ops []types.DMSConfigOp) (map[string]string, error)
	// SetParameters applies the given operations via dms-cli (one invocation per op).
	SetParameters(ops []types.DMSConfigOp) error
	// InstallBFB installs the BFB file with the new firmware version on a BlueField device
	InstallBFB(ctx context.Context, version string, bfbPath string) error
}

// dmsClient implements the DMSClient interface
type dmsClient struct {
	device        v1alpha1.NicDeviceStatus
	targetPCI     string
	bindAddress   string
	authParams    []string
	execInterface execUtils.Interface
}

// target returns the dms-cli target identity for this device (pci/<BDF>).
func (i *dmsClient) target() string {
	return "pci/" + i.targetPCI
}

// baseArgs returns the leading dms-cli args common to all invocations:
//
//	dms-cli -a <addr> <authParams…> -t pci/<BDF> --timeout <t>
func (i *dmsClient) baseArgs() []string {
	args := append([]string{dmsClientPath, "-a", i.bindAddress}, i.authParams...)
	return append(args, "-t", i.target(), "--timeout", dmsClientTimeout)
}

// runSet applies one or more `<leaf>=<value>` assignments under a YANG container path:
//
//	dms-cli … <path> <leaf>=<value> [<leaf>=<value> …]
func (i *dmsClient) runSet(path string, assignments []string) error {
	args := append(i.baseArgs(), path)
	args = append(args, assignments...)
	log.Log.V(2).Info("dmsClient.runSet()", "device", i.device.SerialNumber, "args", strings.Join(args, " "))

	command := i.execInterface.Command(args[0], args[1:]...)
	output, err := command.CombinedOutput()
	log.Log.V(2).Info("dmsClient.runSet() output", "device", i.device.SerialNumber, "output", string(output))
	if err != nil {
		return fmt.Errorf("failed to set %s: %v, output: %s", path, err, string(output))
	}
	return nil
}

// runGetLeaf reads a single leaf and returns its value:
//
//	dms-cli … <path> <leaf> --plain      ->  "<leaf>: <value>"
func (i *dmsClient) runGetLeaf(path, leaf string) (string, error) {
	args := append(i.baseArgs(), path, leaf, "--plain")
	log.Log.V(2).Info("dmsClient.runGetLeaf()", "device", i.device.SerialNumber, "args", strings.Join(args, " "))

	command := i.execInterface.Command(args[0], args[1:]...)
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get %s/%s: %v", path, leaf, err)
	}
	return parsePlainValue(string(output), leaf), nil
}

// parsePlainValue extracts a leaf value from dms-cli `--plain` output, which is
// `key: value` lines. It returns the value for the matching leaf, falling back to the
// last non-empty line's value (or the trimmed output) when no key matches.
func parsePlainValue(output, leaf string) string {
	var fallback string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			fallback = line
			continue
		}
		value = strings.TrimSpace(value)
		fallback = value
		if strings.TrimSpace(key) == leaf {
			return value
		}
	}
	return fallback
}

// SetParameters applies each operation as one dms-cli invocation. Within an op, leaves
// are emitted in sorted order for deterministic commands.
func (i *dmsClient) SetParameters(ops []types.DMSConfigOp) error {
	log.Log.V(2).Info("dmsClient.SetParameters()", "device", i.device.SerialNumber, "ops", len(ops))

	for _, op := range ops {
		assignments, err := assignmentsForOp(op)
		if err != nil {
			return err
		}
		if len(assignments) == 0 {
			continue
		}
		if err := i.runSet(op.Path, assignments); err != nil {
			return err
		}
	}
	return nil
}

// GetParameters reads every leaf referenced by the operations and returns their current
// values keyed by "<path>/<leaf>".
func (i *dmsClient) GetParameters(ops []types.DMSConfigOp) (map[string]string, error) {
	log.Log.V(2).Info("dmsClient.GetParameters()", "device", i.device.SerialNumber, "ops", len(ops))

	values := make(map[string]string)
	for _, op := range ops {
		for _, leaf := range sortedKeys(op.Values) {
			value, err := i.runGetLeaf(op.Path, leaf)
			if err != nil {
				return nil, err
			}
			values[op.Path+"/"+leaf] = value
		}
	}
	log.Log.V(2).Info("dmsClient.GetParameters() values", "device", i.device.SerialNumber, "values", values)
	return values, nil
}

// OpsApplied reports whether every leaf of the given ops currently holds its desired
// value (read via the client). When the same leaf is set more than once across the ops
// (e.g. an admin-status down->up toggle), only the final value is verified — apply runs
// the full ordered sequence, this check verifies the resulting steady state.
func OpsApplied(client DMSClient, ops []types.DMSConfigOp) (bool, error) {
	if len(ops) == 0 {
		return true, nil
	}

	// Desired final value per "<path>/<leaf>", last write wins.
	desired := make(map[string]string)
	for _, op := range ops {
		for leaf, v := range op.Values {
			s, err := types.StringifyDMSValue(v)
			if err != nil {
				return false, err
			}
			desired[op.Path+"/"+leaf] = s
		}
	}

	current, err := client.GetParameters(ops)
	if err != nil {
		return false, err
	}

	for key, want := range desired {
		if current[key] != want {
			log.Log.V(2).Info("OpsApplied(): leaf not applied", "leaf", key, "want", want, "got", current[key])
			return false, nil
		}
	}
	log.Log.V(2).Info("OpsApplied(): all leaves applied", "leaves", len(desired))
	return true, nil
}

// assignmentsForOp renders an op's leaves into sorted "<leaf>=<value>" assignments.
func assignmentsForOp(op types.DMSConfigOp) ([]string, error) {
	keys := sortedKeys(op.Values)
	assignments := make([]string, 0, len(keys))
	for _, leaf := range keys {
		value, err := types.StringifyDMSValue(op.Values[leaf])
		if err != nil {
			return nil, fmt.Errorf("op %q leaf %q: %w", op.Path, leaf, err)
		}
		assignments = append(assignments, fmt.Sprintf("%s=%s", leaf, value))
	}
	return assignments, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// GetQoSSettings returns the current QoS settings (trust mode, PFC, ToS) for the device.
// interfaceName is retained for API compatibility; in the target-based model the values
// are read from the device target. Per-port selectors are an open item.
func (i *dmsClient) GetQoSSettings(interfaceName string) (*v1alpha1.QosSpec, error) {
	log.Log.V(2).Info("dmsClient.GetQoSSettings()", "interfaceName", interfaceName, "device", i.device.SerialNumber)

	trust, err := i.runGetLeaf(qosPath, qosTrustModeLeaf)
	if err != nil {
		return nil, fmt.Errorf("failed to get trust mode: %w", err)
	}

	pfc, err := i.runGetLeaf(qosPFCPath, qosPFCLeaf)
	if err != nil {
		return nil, fmt.Errorf("failed to get PFC configuration: %w", err)
	}

	tos, err := i.runGetLeaf(roceToSPath, roceToSLeaf)
	if err != nil {
		return nil, fmt.Errorf("failed to get ToS configuration: %w", err)
	}
	tosValue, err := strconv.Atoi(strings.TrimSpace(tos))
	if err != nil {
		return nil, fmt.Errorf("failed to convert ToS %q to int: %w", tos, err)
	}

	return &v1alpha1.QosSpec{
		Trust: trust,
		PFC:   prioritiesToPFCMask(pfc),
		ToS:   tosValue,
	}, nil
}

// SetQoSSettings applies the QoS settings (trust mode, PFC, ToS) to the device target.
func (i *dmsClient) SetQoSSettings(spec *v1alpha1.QosSpec) error {
	log.Log.V(2).Info("dmsClient.SetQoSSettings()", "spec", spec, "device", i.device.SerialNumber)

	switch spec.Trust {
	case consts.TrustModeDscp, consts.TrustModePfc:
	default:
		return fmt.Errorf("invalid trust mode: %s", spec.Trust)
	}

	if err := i.runSet(qosPath, []string{qosTrustModeLeaf + "=" + spec.Trust}); err != nil {
		return fmt.Errorf("failed to set trust mode: %w", err)
	}

	if err := i.runSet(qosPFCPath, []string{qosPFCLeaf + "=" + pfcMaskToPriorities(spec.PFC)}); err != nil {
		return fmt.Errorf("failed to set PFC configuration: %w", err)
	}

	if spec.ToS != 0 {
		if err := i.runSet(roceToSPath, []string{roceToSLeaf + "=" + strconv.Itoa(spec.ToS)}); err != nil {
			return fmt.Errorf("failed to set ToS configuration: %w", err)
		}
	}
	return nil
}

// pfcMaskToPriorities converts the operator's PFC mask ("0,0,0,1,0,0,0,0" — or the
// digit form "00010000") into the dms-cli enabled-priorities list "[3]".
// Open item: confirm the enabled-priorities encoding against real HW.
func pfcMaskToPriorities(mask string) string {
	digits := strings.ReplaceAll(mask, ",", "")
	var priorities []string
	for idx, r := range digits {
		if r != '0' {
			priorities = append(priorities, strconv.Itoa(idx))
		}
	}
	return "[" + strings.Join(priorities, ",") + "]"
}

// prioritiesToPFCMask is the inverse of pfcMaskToPriorities: it converts an
// enabled-priorities list "[3]" back into the 8-priority comma mask "0,0,0,1,0,0,0,0".
func prioritiesToPFCMask(priorities string) string {
	mask := make([]string, 8)
	for idx := range mask {
		mask[idx] = "0"
	}
	trimmed := strings.Trim(strings.TrimSpace(priorities), "[]")
	if trimmed != "" {
		for _, p := range strings.Split(trimmed, ",") {
			if idx, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && idx >= 0 && idx < len(mask) {
				mask[idx] = "1"
			}
		}
	}
	return strings.Join(mask, ",")
}

// InstallBFB installs the BFB file with the new firmware version on a BlueField device.
// OS install/activate are routed through dms-cli; the exact gNOI action form is an open
// item pending the T1/T2 OS lifecycle docs.
func (i *dmsClient) InstallBFB(ctx context.Context, version string, bfbPath string) error {
	log.Log.V(2).Info("dmsClient.InstallBFB()", "version", version, "bfbPath", bfbPath, "device", i.device.SerialNumber)

	if !utils.IsBlueFieldDevice(i.device.Type) {
		err := fmt.Errorf("cannot install BFB file on non-BlueField device")
		log.Log.Error(err, "failed to install BFB", "device", i.device.SerialNumber, "deviceType", i.device.Type)
		return err
	}

	installArgs := append([]string{dmsClientPath, "-a", i.bindAddress}, i.authParams...)
	installArgs = append(installArgs, "-t", i.target(), "os", "install", "--version", version, "--pkg", bfbPath)
	log.Log.V(2).Info("dmsClient.InstallBFB() install command", "args", strings.Join(installArgs, " "))
	command := i.execInterface.CommandContext(ctx, installArgs[0], installArgs[1:]...)
	output, err := utils.RunCommand(command)
	if err != nil {
		log.Log.Error(err, "failed to install BFB", "device", i.device.SerialNumber, "deviceType", i.device.Type)
		return err
	}
	log.Log.V(2).Info("BFB installed successfully", "device", i.device.SerialNumber, "version", version, "output", string(output))

	activateArgs := append([]string{dmsClientPath, "-a", i.bindAddress}, i.authParams...)
	activateArgs = append(activateArgs, "-t", i.target(), "os", "activate", "--version", version)
	log.Log.V(2).Info("dmsClient.InstallBFB() activate command", "args", strings.Join(activateArgs, " "))
	command = i.execInterface.CommandContext(ctx, activateArgs[0], activateArgs[1:]...)
	output, err = utils.RunCommand(command)
	if err != nil {
		log.Log.Error(err, "failed to activate BFB", "device", i.device.SerialNumber, "deviceType", i.device.Type)
		return err
	}
	log.Log.V(2).Info("BFB activated successfully", "device", i.device.SerialNumber, "version", version, "output", string(output))

	return nil
}
