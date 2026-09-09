// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"github.com/spf13/cobra"
	"github.com/zarhus/benchctl/platform"
)

func diskCmd() *cobra.Command {
	disk := &cobra.Command{
		Use:   "disk",
		Short: "Attach a disk/installer image for the host to boot from",
	}

	var mountMedium string
	mount := &cobra.Command{
		Use:   "mount <image>",
		Short: "Record a disk image to attach from the next power on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			medium, err := platform.ParseDiskMedium(mountMedium)
			if err != nil {
				return err
			}
			return withPlatform(func(p platform.Platform) error {
				return p.DiskMount(args[0], medium)
			})
		},
	}
	// virtio is the default rather than ahci: on rdv3, mounting an image via
	// pcie_group_0.pcie4.pcie_rc.ahci0.ahci.image_path reproducibly hits a
	// Synchronous Exception in the reference UEFI's ArmCpuDxe (identical
	// PC/registers regardless of the attached image's own content - Ubuntu
	// Desktop and Alpine both crash at exactly the same instruction), which
	// points at the AHCI probe/enumeration path itself, not the guest image.
	// See examples/rdv3-coreboot-linuxboot/kb/rdv3.md's D6 section.
	mount.Flags().StringVar(&mountMedium, "medium", "virtio", "how the image is presented to the host: virtio (virtio-blk) or ahci (SATA/AHCI - known to crash the reference UEFI on rdv3, see kb/rdv3.md)")

	eject := &cobra.Command{
		Use:   "eject",
		Short: "Forget the recorded disk image",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error {
				return p.DiskEject()
			})
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the recorded disk image, if any",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error {
				return p.DiskStatus()
			})
		},
	}

	disk.AddCommand(mount, eject, status)
	return disk
}
