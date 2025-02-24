/*
2025 NVIDIA CORPORATION & AFFILIATES
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package firmware

import (
	"context"
	"github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
)

// FirmwareManager contains logic for managing NIC devices FW on the host
type FirmwareManager interface {
	// ValidateDeviceFirmwareSource will validate device's firmware source spec for configuration errors
	// returns bool - firmware source valid and ready
	// returns error - there are errors in the firmware source spec
	// Possible errors:
	// Referenced NicFirmwareSource obj doesn't exist -> "Requested NicFirmwareSource object %s doesn't exist"
	// Source exists but not ready / failed -> "Requested NicFirmwareSource object %s is not ready/failed"
	// Source exists and ready but doesn't contain an image for this device's PSID -> "Requested NicFirmwareSource %s has no matching FW image for PSID %s"
	ValidateDeviceFirmwareSource(ctx context.Context, device *v1alpha1.NicDevice) (bool, error)

	// ValidateDeviceFirmwareVersion will validate if the firmware, currently installed on the device, matches the spec
	// returns bool - currently installed fw matched the spec
	// returns error - there were errors while applying nv configuration
	ValidateDeviceFirmwareVersion(ctx context.Context, device *v1alpha1.NicDevice) (bool, error)

	// UpdateDeviceFirmware will update the device's FW to the version requested in spec
	// returns error - there were errors while updating firmware
	UpdateDeviceFirmware(ctx context.Context, device *v1alpha1.NicDevice) error
}
