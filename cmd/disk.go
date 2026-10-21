// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zarhus/benchctl/platform"
)

func diskCmd() *cobra.Command {
	disk := &cobra.Command{
		Use:   "disk",
		Short: "Attach a disk/installer image for the host to boot from",
	}

	var (
		mountMedium        string
		transport          string
		listen, mediaURL   string
		bootOnce, resetBMC bool
		powerOn            bool
	)
	mount := &cobra.Command{
		Use:   "mount <image>",
		Short: "Attach a disk image for the host to boot from",
		Long: `Attach a disk image for the host to boot from.

On the fvp platform, disk mount records a local image to attach from the next
power ac on.

On the bmc platform, the BMC presents the image to the host as virtual media and
a boot override boots it. disk mount powers the host off, attaches the image,
sets the boot override and prints "disk-ready". The host stays off for the
caller to attach the console and run power on, or disk mount powers it on
before "disk-ready" with --power-on. disk mount serves a local file until
SIGINT or SIGTERM, then ejects it. An https:// or nfs:// URL goes to the BMC as
it is, and disk mount returns after "disk-ready".

--transport (or BENCHCTL_MEDIA_TRANSPORT) sets how a local file reaches the BMC:
https, the default, serves it over HTTPS for the BMC to fetch, websocket streams
it over the BMC's NBD websocket, and auto picks websocket where the BMC offers
it and https otherwise. Some OpenBMC virtual media services crash when Redfish
reads the slot during a websocket stream, so websocket suits a BMC known to
have the fix. --listen and --media-url apply to https only.

A signal before "disk-ready" stops the mount at once while it waits, or after
the insert or eject task it is following. A mount that stops or fails after
the insert powers the host off, ejects the image and clears the boot override.

--listen (or BENCHCTL_MEDIA_LISTEN) is the image server's address, :8443 by
default. --media-url (or BENCHCTL_MEDIA_URL) is the URL the BMC fetches the
file under, without the file name. By default it is https:// with this
machine's address toward the BMC and the listen port. Some BMCs fetch only
from port 443: listen on :443, or redirect 443 to the listen port and set
--media-url without a port.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			medium, err := platform.ParseDiskMedium(mountMedium)
			if err != nil {
				return err
			}
			mediaTransport, err := platform.ParseMediaTransport(resolveSetting(transport, "BENCHCTL_MEDIA_TRANSPORT"))
			if err != nil {
				return err
			}
			image := args[0]
			opts := platform.DiskOptions{Medium: medium, BootOnce: bootOnce, ResetBMC: resetBMC, PowerOn: powerOn}
			return withPlatform(func(p platform.Platform) error {
				server, ok := p.(platform.DiskServer)
				if !ok {
					return p.DiskMount(context.Background(), image, opts)
				}
				// A driver that serves images stops a mount on the first
				// signal and undoes it.
				ctx, stop := serveContext(func() {}, "benchctl: interrupted again, exiting without undoing the mount")
				defer stop()
				if strings.Contains(image, "://") {
					return p.DiskMount(ctx, image, opts)
				}
				return server.DiskServe(ctx, image, platform.DiskServeOptions{
					DiskOptions: opts,
					Transport:   mediaTransport,
					Listen:      resolveSettingOr(listen, "BENCHCTL_MEDIA_LISTEN", ":8443"),
					MediaURL:    resolveSetting(mediaURL, "BENCHCTL_MEDIA_URL"),
				})
			})
		},
	}
	flags := mount.Flags()
	flags.StringVar(&mountMedium, "medium", "", "how the image is presented to the host: virtio or ahci on fvp (default virtio, ahci is known to crash the reference UEFI on rdv3, see kb/rdv3.md), cd or usb on bmc (default cd for .iso, usb otherwise)")
	flags.StringVar(&transport, "transport", "", "how a local file reaches the BMC: auto, https or websocket (or BENCHCTL_MEDIA_TRANSPORT; default https)")
	flags.StringVar(&listen, "listen", "", "`address` the image server listens on (or BENCHCTL_MEDIA_LISTEN; default :8443)")
	flags.StringVar(&mediaURL, "media-url", "", "`URL` the BMC fetches the image under, without the file name (or BENCHCTL_MEDIA_URL)")
	flags.BoolVar(&bootOnce, "boot-once", false, "boot the image once instead of until disk eject")
	flags.BoolVar(&resetBMC, "reset-bmc", false, "reset the BMC once if it offers no media slots")
	flags.BoolVar(&powerOn, "power-on", false, "power the host on once the image and the boot override are in place")

	var slot string
	eject := &cobra.Command{
		Use:   "eject",
		Short: "Detach the disk image",
		Long: `Detach the disk image.

On the bmc platform, disk eject ejects every inserted virtual media slot that
has an EjectMedia action, or only --slot, and clears the boot override. That
includes the slot of a mount over the NBD websocket, which then ends.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withPlatform(func(p platform.Platform) error {
				return p.DiskEject(slot)
			})
		},
	}
	eject.Flags().StringVar(&slot, "slot", "", "eject only the virtual media slot with this `ID`")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the attached or recorded disk image",
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
