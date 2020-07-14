//  Copyright 2020  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package importer

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/compute-image-tools/daisy"
	"google.golang.org/api/compute/v1"

	"github.com/GoogleCloudPlatform/compute-image-tools/cli_tools/common/imagefile"
	daisy_utils "github.com/GoogleCloudPlatform/compute-image-tools/cli_tools/common/utils/daisy"
	string_utils "github.com/GoogleCloudPlatform/compute-image-tools/cli_tools/common/utils/string"
	"github.com/GoogleCloudPlatform/compute-image-tools/cli_tools/daisycommon"
)

const (
	inflateFilePath  = "inflate_file.wf.json"
	inflateImagePath = "inflate_image.wf.json"

	// When exceeded, we use default values for PDs, rather than more accurate
	// values used by inspection. When using default values, the worker may
	// need to resize the PDs, which requires escalated privileges.
	inspectionTimeout = time.Second * 3

	// 10GB is the default disk size used in inflate_file.wf.json.
	defaultInflationDiskSizeGB = 10
)

// inflater constructs a new persistentDisk, typically starting from a
// frozen representation of a disk, such as a VMDK file or a GCP disk image.
//
// Implementers can expose detailed logs using the traceLogs() method.
type inflater interface {
	inflate() (persistentDisk, error)
	traceLogs() []string
	cancel(reason string) bool
}

// daisyInflater implements an inflater using daisy workflows, and is capable
// of inflating GCP disk images and qemu-img compatible disk files.
type daisyInflater struct {
	wf              *daisy.Workflow
	inflatedDiskURI string
	serialLogs      []string
}

func (inflater *daisyInflater) inflate() (persistentDisk, error) {
	err := inflater.wf.Run(context.Background())
	if inflater.wf.Logger != nil {
		inflater.serialLogs = inflater.wf.Logger.ReadSerialPortLogs()
	}
	// See `daisy_workflows/image_import/import_image.sh` for generation of these values.
	targetSizeGB := inflater.wf.GetSerialConsoleOutputValue("target-size-gb")
	sourceSizeGB := inflater.wf.GetSerialConsoleOutputValue("source-size-gb")
	importFileFormat := inflater.wf.GetSerialConsoleOutputValue("import-file-format")
	return persistentDisk{
		uri:        inflater.inflatedDiskURI,
		sizeGb:     string_utils.SafeStringToInt(targetSizeGB),
		sourceGb:   string_utils.SafeStringToInt(sourceSizeGB),
		sourceType: importFileFormat,
	}, err
}

type persistentDisk struct {
	uri        string
	sizeGb     int64
	sourceGb   int64
	sourceType string
}

func createDaisyInflater(args ImportArguments, fileInspector imagefile.Inspector) (inflater, error) {
	diskName := "disk-" + args.ExecutionID
	var wfPath string
	var vars map[string]string
	var inflationDiskIndex int
	if isImage(args.Source) {
		wfPath = inflateImagePath
		vars = map[string]string{
			"source_image": args.Source.Path(),
			"disk_name":    diskName,
		}
		inflationDiskIndex = 0 // Workflow only uses one disk.
	} else {
		wfPath = inflateFilePath
		vars = createDaisyVarsForFile(args, fileInspector, diskName)
		inflationDiskIndex = 1 // First disk is for the worker
	}

	wf, err := daisycommon.ParseWorkflow(path.Join(args.WorkflowDir, wfPath), vars,
		args.Project, args.Zone, args.ScratchBucketGcsPath, args.Oauth, args.Timeout.String(), args.ComputeEndpoint,
		args.GcsLogsDisabled, args.CloudLogsDisabled, args.StdoutLogsDisabled)
	if err != nil {
		return nil, err
	}

	daisy_utils.UpdateAllInstanceNoExternalIP(wf, args.NoExternalIP)
	for k, v := range vars {
		wf.AddVar(k, v)
	}

	if strings.Contains(args.OS, "windows") {
		addFeatureToDisk(wf, "WINDOWS", inflationDiskIndex)
	}

	return &daisyInflater{
		wf:              wf,
		inflatedDiskURI: fmt.Sprintf("zones/%s/disks/%s", args.Zone, diskName),
	}, nil
}

func (inflater *daisyInflater) traceLogs() []string {
	return inflater.serialLogs
}

// addFeatureToDisk finds the first `CreateDisk` step, and adds `feature` as
// a guestOsFeature to the disk at index `diskIndex`.
func addFeatureToDisk(workflow *daisy.Workflow, feature string, diskIndex int) {
	disk := getDisk(workflow, diskIndex)
	disk.GuestOsFeatures = append(disk.GuestOsFeatures, &compute.GuestOsFeature{
		Type: feature,
	})
}

func getDisk(workflow *daisy.Workflow, diskIndex int) *daisy.Disk {
	for _, step := range workflow.Steps {
		if step.CreateDisks != nil {
			disks := *step.CreateDisks
			if diskIndex < len(disks) {
				return disks[diskIndex]
			}
			panic(fmt.Sprintf("CreateDisks step did not have disk at index %d", diskIndex))
		}
	}

	panic("Did not find CreateDisks step.")
}

func (inflater *daisyInflater) cancel(reason string) bool {
	inflater.wf.CancelWithReason(reason)
	return true
}

func createDaisyVarsForFile(args ImportArguments,
	fileInspector imagefile.Inspector, diskName string) map[string]string {
	vars := map[string]string{
		"source_disk_file": args.Source.Path(),
		"import_network":   args.Network,
		"import_subnet":    args.Subnet,
		"disk_name":        diskName,
	}

	// To reduce the runtime permissions used on the inflation worker, we pre-allocate
	// disks sufficient to hold the disk file and the inflated disk. If inspection fails,
	// then the default values in the daisy workflow will be used. The scratch disk gets
	// a padding factor to account for filesystem overhead.
	deadline, cancelFunc := context.WithDeadline(context.Background(), time.Now().Add(inspectionTimeout))
	defer cancelFunc()
	metadata, err := fileInspector.Inspect(deadline, args.Source.Path())
	if err == nil {
		vars["inflated_disk_size_gb"] = fmt.Sprintf("%d", calculateInflatedSize(metadata))
		vars["scratch_disk_size_gb"] = fmt.Sprintf("%d", calculateScratchDiskSize(metadata))
	}
	return vars
}

// Allocate extra room for filesystem overhead, and
// ensure a minimum of 10GB (the minimum size of a GCP disk).
func calculateScratchDiskSize(metadata imagefile.Metadata) int64 {
	// This uses the historic padding calculation from import_image.sh: add ten percent,
	// and round up.
	padded := int64(float64(metadata.PhysicalSizeGB)*1.1) + 1
	if padded < defaultInflationDiskSizeGB {
		return defaultInflationDiskSizeGB
	}
	return padded
}

// Ensure a minimum of 10GB (the minimum size of a GCP disk)
func calculateInflatedSize(metadata imagefile.Metadata) int64 {
	if metadata.VirtualSizeGB < defaultInflationDiskSizeGB {
		return defaultInflationDiskSizeGB
	}
	return metadata.VirtualSizeGB
}