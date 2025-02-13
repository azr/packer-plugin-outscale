package bsusurrogate

import (
	"context"
	"fmt"

	"github.com/antihax/optional"
	osccommon "github.com/hashicorp/packer-plugin-outscale/builder/osc/common"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/outscale/osc-sdk-go/osc"
)

// StepRegisterOMI creates the OMI.
type StepRegisterOMI struct {
	RootDevice    RootBlockDevice
	OMIDevices    []osc.BlockDeviceMappingImage
	LaunchDevices []osc.BlockDeviceMappingVmCreation
	image         *osc.Image
	RawRegion     string
}

func (s *StepRegisterOMI) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("config").(*Config)
	oscconn := state.Get("osc").(*osc.APIClient)
	snapshotIds := state.Get("snapshot_ids").(map[string]string)
	ui := state.Get("ui").(packersdk.Ui)

	ui.Say("Registering the OMI...")

	blockDevices := s.combineDevices(snapshotIds)

	registerOpts := osc.CreateImageRequest{
		ImageName:           config.OMIName,
		Architecture:        "x86_64",
		RootDeviceName:      s.RootDevice.DeviceName,
		BlockDeviceMappings: blockDevices,
	}

	if config.OMIDescription != "" {
		registerOpts.Description = config.OMIDescription
	}
	registerResp, _, err := oscconn.ImageApi.CreateImage(context.Background(), &osc.CreateImageOpts{
		CreateImageRequest: optional.NewInterface(registerOpts),
	})
	if err != nil {
		state.Put("error", fmt.Errorf("Error registering OMI: %s", err))
		ui.Error(state.Get("error").(error).Error())
		return multistep.ActionHalt
	}

	// Set the OMI ID in the state
	ui.Say(fmt.Sprintf("OMI: %s", registerResp.Image.ImageId))
	omis := make(map[string]string)
	omis[s.RawRegion] = registerResp.Image.ImageId
	state.Put("omis", omis)

	// Wait for the image to become ready
	ui.Say("Waiting for OMI to become ready...")
	if err := osccommon.WaitUntilOscImageAvailable(oscconn, registerResp.Image.ImageId); err != nil {
		err := fmt.Errorf("Error waiting for OMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	imagesResp, _, err := oscconn.ImageApi.ReadImages(context.Background(), &osc.ReadImagesOpts{
		ReadImagesRequest: optional.NewInterface(osc.ReadImagesRequest{
			Filters: osc.FiltersImage{
				ImageIds: []string{registerResp.Image.ImageId},
			},
		}),
	})

	if err != nil {
		err := fmt.Errorf("Error searching for OMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	s.image = &imagesResp.Images[0]

	snapshots := make(map[string][]string)
	for _, blockDeviceMapping := range imagesResp.Images[0].BlockDeviceMappings {
		if blockDeviceMapping.Bsu.SnapshotId != "" {
			snapshots[s.RawRegion] = append(snapshots[s.RawRegion], blockDeviceMapping.Bsu.SnapshotId)
		}
	}
	state.Put("snapshots", snapshots)

	return multistep.ActionContinue
}

func (s *StepRegisterOMI) Cleanup(state multistep.StateBag) {
	if s.image == nil {
		return
	}

	_, cancelled := state.GetOk(multistep.StateCancelled)
	_, halted := state.GetOk(multistep.StateHalted)
	if !cancelled && !halted {
		return
	}

	oscconn := state.Get("osc").(*osc.APIClient)
	ui := state.Get("ui").(packersdk.Ui)

	ui.Say("Deregistering the OMI because cancellation or error...")
	deregisterOpts := osc.DeleteImageRequest{ImageId: s.image.ImageId}
	_, _, err := oscconn.ImageApi.DeleteImage(context.Background(), &osc.DeleteImageOpts{
		DeleteImageRequest: optional.NewInterface(deregisterOpts),
	})

	if err != nil {
		ui.Error(fmt.Sprintf("Error deregistering OMI, may still be around: %s", err))
		return
	}
}

func (s *StepRegisterOMI) combineDevices(snapshotIDs map[string]string) []osc.BlockDeviceMappingImage {
	devices := map[string]osc.BlockDeviceMappingImage{}

	for _, device := range s.OMIDevices {
		devices[device.DeviceName] = device
	}

	// Devices in launch_block_device_mappings override any with
	// the same name in ami_block_device_mappings, except for the
	// one designated as the root device in ami_root_device
	for _, device := range s.LaunchDevices {
		snapshotID, ok := snapshotIDs[device.DeviceName]
		if ok {
			device.Bsu.SnapshotId = snapshotID
		}
		if device.DeviceName == s.RootDevice.SourceDeviceName {
			device.DeviceName = s.RootDevice.DeviceName

			if device.Bsu.VolumeType != "" {
				device.Bsu.VolumeType = s.RootDevice.VolumeType
				if device.Bsu.VolumeType != "io1" {
					device.Bsu.Iops = 0
				}
			}

		}
		devices[device.DeviceName] = copyToDeviceMappingImage(device)
	}

	blockDevices := []osc.BlockDeviceMappingImage{}
	for _, device := range devices {
		blockDevices = append(blockDevices, device)
	}
	return blockDevices
}

func copyToDeviceMappingImage(device osc.BlockDeviceMappingVmCreation) osc.BlockDeviceMappingImage {
	deviceImage := osc.BlockDeviceMappingImage{
		DeviceName:        device.DeviceName,
		VirtualDeviceName: device.VirtualDeviceName,
		Bsu: osc.BsuToCreate{
			DeleteOnVmDeletion: device.Bsu.DeleteOnVmDeletion,
			Iops:               device.Bsu.Iops,
			SnapshotId:         device.Bsu.SnapshotId,
			VolumeSize:         device.Bsu.VolumeSize,
			VolumeType:         device.Bsu.VolumeType,
		},
	}
	return deviceImage
}
