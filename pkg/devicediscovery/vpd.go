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

package devicediscovery

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Mellanox/nic-configuration-operator/pkg/types"
)

const (
	vpdLargeResourceFlag             = 0x80
	vpdLargeResourceHeaderSize       = 3
	vpdLargeResourceIdentifierString = 0x82
	vpdLargeResourceReadOnlyData     = 0x90
	vpdSmallResourceTypeShift        = 3
	vpdSmallResourceTypeEndTag       = 0x0f
	vpdSmallResourceLengthMask       = 0x07
	vpdKeywordHeaderSize             = 3
)

// GetVPD retrieves the PCI VPD identifier string, part number, and serial
// number through the kernel's sysfs interface. The kernel transparently routes
// VPD access to function 0 for devices whose PCI functions share VPD storage.
func (d *deviceDiscoveryUtils) GetVPD(pciAddr string) (*types.VPD, error) {
	log.Log.Info("HostUtils.GetVPD()", "pciAddr", pciAddr)

	return getVPDFromPath(pciDevicesPath, pciAddr)
}

func getVPDFromPath(basePath, pciAddr string) (*types.VPD, error) {
	if pciAddr == "" || filepath.Base(pciAddr) != pciAddr || pciAddr == "." || pciAddr == ".." {
		return nil, fmt.Errorf("invalid PCI address %q", pciAddr)
	}

	vpdPath := filepath.Join(basePath, pciAddr, "vpd")
	data, err := os.ReadFile(vpdPath)
	if err != nil {
		return nil, fmt.Errorf("reading PCI VPD for %s from %s: %w", pciAddr, vpdPath, err)
	}

	vpd, err := parsePCIVPD(data)
	if err != nil {
		return nil, fmt.Errorf("parsing PCI VPD for %s from %s: %w", pciAddr, vpdPath, err)
	}
	return vpd, nil
}

// parsePCIVPD parses the resource records defined by the PCI VPD format. It
// extracts the Identifier String resource as ModelName and the PN/SN keywords
// from the read-only data resource. Unknown resources and keywords are skipped.
func parsePCIVPD(data []byte) (*types.VPD, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("VPD data is empty")
	}

	var modelName, partNumber, serialNumber string
	foundReadOnlyData := false
	foundEndTag := false

	for offset := 0; offset < len(data); {
		resourceOffset := offset
		tag := data[offset]

		if tag&vpdLargeResourceFlag != 0 {
			if len(data)-offset < vpdLargeResourceHeaderSize {
				return nil, fmt.Errorf("truncated large-resource header at offset %d", resourceOffset)
			}

			resourceLength := int(binary.LittleEndian.Uint16(data[offset+1 : offset+vpdLargeResourceHeaderSize]))
			valueOffset := offset + vpdLargeResourceHeaderSize
			if resourceLength > len(data)-valueOffset {
				return nil, fmt.Errorf(
					"large resource %#02x at offset %d declares %d bytes, only %d remain",
					tag, resourceOffset, resourceLength, len(data)-valueOffset,
				)
			}

			value := data[valueOffset : valueOffset+resourceLength]
			switch tag {
			case vpdLargeResourceIdentifierString:
				var err error
				modelName, err = parseVPDText("identifier string", value)
				if err != nil {
					return nil, fmt.Errorf("resource at offset %d: %w", resourceOffset, err)
				}
			case vpdLargeResourceReadOnlyData:
				foundReadOnlyData = true
				fields, err := parseVPDKeywords(value, valueOffset)
				if err != nil {
					return nil, err
				}
				if value, ok := fields["PN"]; ok {
					partNumber, err = parseVPDText("PN", value)
					if err != nil {
						return nil, err
					}
				}
				if value, ok := fields["SN"]; ok {
					serialNumber, err = parseVPDText("SN", value)
					if err != nil {
						return nil, err
					}
				}
			}

			offset = valueOffset + resourceLength
			continue
		}

		resourceLength := int(tag & vpdSmallResourceLengthMask)
		valueOffset := offset + 1
		if resourceLength > len(data)-valueOffset {
			return nil, fmt.Errorf(
				"small resource %#02x at offset %d declares %d bytes, only %d remain",
				tag, resourceOffset, resourceLength, len(data)-valueOffset,
			)
		}
		offset = valueOffset + resourceLength

		if tag>>vpdSmallResourceTypeShift == vpdSmallResourceTypeEndTag {
			foundEndTag = true
			break
		}
	}

	if !foundEndTag {
		return nil, fmt.Errorf("VPD data is missing the end tag")
	}
	if !foundReadOnlyData {
		return nil, fmt.Errorf("VPD data is missing the read-only data resource")
	}

	missing := make([]string, 0, 2)
	if partNumber == "" {
		missing = append(missing, "PN")
	}
	if serialNumber == "" {
		missing = append(missing, "SN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("VPD read-only data is missing required keyword(s): %s", strings.Join(missing, ", "))
	}

	return &types.VPD{
		PartNumber:   partNumber,
		SerialNumber: serialNumber,
		ModelName:    modelName,
	}, nil
}

func parseVPDKeywords(data []byte, absoluteOffset int) (map[string][]byte, error) {
	fields := make(map[string][]byte)
	for offset := 0; offset < len(data); {
		if len(data)-offset < vpdKeywordHeaderSize {
			return nil, fmt.Errorf("truncated VPD keyword header at offset %d", absoluteOffset+offset)
		}

		keyword := string(data[offset : offset+2])
		valueLength := int(data[offset+2])
		valueOffset := offset + vpdKeywordHeaderSize
		if valueLength > len(data)-valueOffset {
			return nil, fmt.Errorf(
				"VPD keyword %q at offset %d declares %d bytes, only %d remain",
				keyword, absoluteOffset+offset, valueLength, len(data)-valueOffset,
			)
		}

		fields[keyword] = data[valueOffset : valueOffset+valueLength]
		offset = valueOffset + valueLength
	}
	return fields, nil
}

func parseVPDText(field string, value []byte) (string, error) {
	value = []byte(strings.Trim(string(value), " \t\r\n\x00"))
	if !utf8.Valid(value) {
		return "", fmt.Errorf("VPD field %s contains invalid UTF-8", field)
	}
	return string(value), nil
}
