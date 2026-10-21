// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"
)

// Virtual media on the bmc driver: the BMC presents an image to the host as a
// read-only CD or USB stick, and a boot override boots it. gofish reads the
// slots and posts the media actions. benchctl follows tasks, sets the boot
// override and handles remote media on AMI MegaRAC itself.

// errNoSlots reports a BMC that offers no slot to insert media into.
var errNoSlots = errors.New("remote media is on but the BMC offers no virtual media slots")

// Message IDs, matched by suffix, that change what the driver does.
const (
	msgInsertMediaError = ".InsertMediaError"                              // AMI: the BMC could not fetch the image
	msgHostBooting      = ".ServiceTemporarilyUnavailableDueToHostBooting" // AMI: no boot override while the host boots
	msgSupportEnabled   = ".SupportEnabled"                                // AMI: remote media is already on
)

// mediaClient connects gofish to the BMC on first use, over the same HTTP
// client as the power requests. It connects with a background context, since
// gofish checks that context on every request and the eject after a signal
// runs once the serve context has ended.
func (b *bmcPlatform) mediaClient() (*gofish.APIClient, error) {
	if b.configErr != nil {
		return nil, b.configErr
	}
	if b.media == nil {
		client, err := gofish.ConnectContext(context.Background(), gofish.ClientConfig{
			Endpoint:          b.redfish.BaseURL,
			HTTPClient:        b.redfish.HTTP,
			Username:          b.access.User,
			Password:          b.access.Password,
			BasicAuth:         true,
			NoModifyTransport: true,
		})
		if err != nil {
			return nil, fmt.Errorf("connecting to Redfish at %s: %w", b.access.IP, redfishError(err))
		}
		b.media = client
	}
	return b.media, nil
}

// mediaSystem reads the ComputerSystem through gofish, fresh on every call so
// its etag is current.
func (b *bmcPlatform) mediaSystem() (*schemas.ComputerSystem, error) {
	client, err := b.mediaClient()
	if err != nil {
		return nil, err
	}
	if err := b.resolveSystem(); err != nil {
		return nil, err
	}
	system, err := schemas.GetComputerSystem(client, b.systemPath)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", b.systemPath, redfishError(err))
	}
	return system, nil
}

// mediaManagers reads the system and returns it with its managers, or every
// manager when the system links none.
func (b *bmcPlatform) mediaManagers() (*schemas.ComputerSystem, []*schemas.Manager, error) {
	system, err := b.mediaSystem()
	if err != nil {
		return nil, nil, err
	}
	managers, err := system.ManagedBy()
	if err != nil {
		return nil, nil, fmt.Errorf("reading the managers of %s: %w", b.systemPath, redfishError(err))
	}
	if len(managers) == 0 {
		if managers, err = b.media.Service.Managers(); err != nil {
			return nil, nil, fmt.Errorf("reading the managers: %w", redfishError(err))
		}
	}
	return system, managers, nil
}

// listSlots returns the virtual media slots the System and its Managers link,
// in that order, each once.
func (b *bmcPlatform) listSlots() ([]*schemas.VirtualMedia, error) {
	system, managers, err := b.mediaManagers()
	if err != nil {
		return nil, err
	}
	var slots []*schemas.VirtualMedia
	seen := map[string]bool{}
	add := func(owner string, list []*schemas.VirtualMedia, err error) error {
		if err != nil {
			return fmt.Errorf("reading the virtual media of %s: %w", owner, redfishError(err))
		}
		for _, slot := range list {
			if !seen[slot.ODataID] {
				seen[slot.ODataID] = true
				slots = append(slots, slot)
			}
		}
		return nil
	}
	list, err := system.VirtualMedia()
	if err := add(system.ODataID, list, err); err != nil {
		return nil, err
	}
	for _, manager := range managers {
		list, err := manager.VirtualMedia()
		if err := add(manager.ODataID, list, err); err != nil {
			return nil, err
		}
	}
	return slots, nil
}

// inserted reports whether slot holds media.
func inserted(slot *schemas.VirtualMedia) bool {
	return slot.Inserted != nil && *slot.Inserted
}

// amiManager is the part of a Manager that remote media and the BMC reset use.
type amiManager struct {
	Oem struct {
		Ami struct {
			VirtualMedia struct {
				RMediaStatus string `json:"RMediaStatus"`
			} `json:"VirtualMedia"`
		} `json:"Ami"`
	} `json:"Oem"`
	Actions struct {
		Reset struct {
			Target  string   `json:"target"`
			Allowed []string `json:"ResetType@Redfish.AllowableValues"`
		} `json:"#Manager.Reset"`
		Oem map[string]struct {
			Target string `json:"target"`
		} `json:"Oem"`
	} `json:"Actions"`
}

// readManager reads the manager at path for its OEM remote media status and
// its actions.
func (b *bmcPlatform) readManager(path string) (amiManager, error) {
	var manager amiManager
	err := b.redfish.Get(path, &manager)
	return manager, err
}

// enableRemoteMedia turns AMI remote media on where a manager reports it
// "Disabled", through the manager's EnableRMedia action, and waits until the
// manager reports it "Enabled".
func (b *bmcPlatform) enableRemoteMedia(ctx context.Context, managers []*schemas.Manager) error {
	for _, manager := range managers {
		state, err := b.readManager(manager.ODataID)
		if err != nil {
			return err
		}
		if state.Oem.Ami.VirtualMedia.RMediaStatus != "Disabled" {
			continue
		}
		target := ""
		for name, action := range state.Actions.Oem {
			if strings.HasSuffix(name, ".EnableRMedia") {
				target = action.Target
			}
		}
		if target == "" {
			return fmt.Errorf("remote media is disabled on the BMC and %s offers no EnableRMedia action: enable it in the BMC's web interface", manager.ODataID)
		}
		fmt.Fprintf(b.progress, "Remote media is disabled on the BMC, enabling it...\n")
		resp, err := b.media.Post(target, map[string]string{"RMediaState": "Enable"})
		if err != nil && !hasMessage(err, msgSupportEnabled) {
			return fmt.Errorf("POST %s: %w", target, redfishError(err))
		}
		if resp != nil {
			resp.Body.Close()
		}
		deadline := time.Now().Add(b.slotsTimeout)
		for {
			state, err := b.readManager(manager.ODataID)
			if err != nil {
				return err
			}
			if state.Oem.Ami.VirtualMedia.RMediaStatus == "Enabled" {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("remote media still %q after %s", state.Oem.Ami.VirtualMedia.RMediaStatus, b.slotsTimeout)
			}
			if err := pause(ctx, b.pollEvery); err != nil {
				return err
			}
		}
		fmt.Fprintln(b.progress, "Remote media is enabled.")
	}
	return nil
}

// insertableSlots enables remote media where it is off and waits up to
// slotsTimeout for slots that take InsertMedia.
func (b *bmcPlatform) insertableSlots(ctx context.Context) ([]*schemas.VirtualMedia, error) {
	_, managers, err := b.mediaManagers()
	if err != nil {
		return nil, err
	}
	if err := b.enableRemoteMedia(ctx, managers); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(b.slotsTimeout)
	for {
		slots, err := b.listSlots()
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(slots, func(slot *schemas.VirtualMedia) bool { return slot.SupportsMediaInsert }) {
			return slots, nil
		}
		if time.Now().After(deadline) {
			return nil, errNoSlots
		}
		if err := pause(ctx, b.pollEvery); err != nil {
			return nil, err
		}
	}
}

// mediaKind is a medium as the BMC names it: the slot's media type and the boot
// override target.
type mediaKind struct {
	mediaType schemas.VirtualMediaType
	idPrefix  string // slot ID prefix for a slot that lists no MediaTypes
	target    schemas.BootSource
}

var (
	kindCD  = mediaKind{schemas.CDVirtualMediaType, "CD", schemas.CdBootSource}
	kindUSB = mediaKind{schemas.USBStickVirtualMediaType, "USBStick", schemas.UsbBootSource}
)

// mediaKindFor picks CD or USB for image: the medium given, or CD for an ISO
// and USB otherwise.
func mediaKindFor(imageURL string, medium DiskMedium) (mediaKind, error) {
	switch medium {
	case DiskCD:
		return kindCD, nil
	case DiskUSB:
		return kindUSB, nil
	case DiskDefault:
		u, err := url.Parse(imageURL)
		if err == nil && strings.EqualFold(path.Ext(u.Path), ".iso") {
			return kindCD, nil
		}
		return kindUSB, nil
	default:
		return mediaKind{}, fmt.Errorf("the bmc platform presents images as cd or usb, not %s", medium)
	}
}

// pickSlot returns a slot of kind that takes InsertMedia, an empty one first.
// A slot fits by its MediaTypes, or by its ID when it lists none.
func pickSlot(slots []*schemas.VirtualMedia, kind mediaKind) (*schemas.VirtualMedia, error) {
	var fitting []*schemas.VirtualMedia
	for _, slot := range slots {
		if slot.SupportsMediaInsert && fits(slot, kind) {
			fitting = append(fitting, slot)
		}
	}
	for _, slot := range fitting {
		if !inserted(slot) {
			return slot, nil
		}
	}
	if len(fitting) > 0 {
		return fitting[0], nil
	}
	return nil, fmt.Errorf("the BMC offers no %s slot that takes InsertMedia (it offers %s)", kind.mediaType, describeSlots(slots))
}

// fits reports whether slot is of kind.
func fits(slot *schemas.VirtualMedia, kind mediaKind) bool {
	return len(slot.MediaTypes) > 0 && slices.Contains(slot.MediaTypes, kind.mediaType) ||
		len(slot.MediaTypes) == 0 && strings.HasPrefix(slot.ID, kind.idPrefix)
}

// describeSlots lists slots with their media types, for an error.
func describeSlots(slots []*schemas.VirtualMedia) string {
	if len(slots) == 0 {
		return "none"
	}
	var parts []string
	for _, slot := range slots {
		note := ""
		if !slot.SupportsMediaInsert {
			note = ", no InsertMedia"
		}
		parts = append(parts, fmt.Sprintf("%s [%s%s]", slot.ID, mediaTypes(slot, " "), note))
	}
	return strings.Join(parts, ", ")
}

// mediaTypes joins the slot's media types with sep.
func mediaTypes(slot *schemas.VirtualMedia, sep string) string {
	types := make([]string, len(slot.MediaTypes))
	for i, t := range slot.MediaTypes {
		types[i] = string(t)
	}
	return strings.Join(types, sep)
}

// transferProtocol maps the image URL's scheme to a TransferProtocolType.
func transferProtocol(imageURL string) (schemas.TransferProtocolType, error) {
	u, err := url.Parse(imageURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		return schemas.HTTPSTransferProtocolType, nil
	case "nfs":
		return schemas.NFSTransferProtocolType, nil
	default:
		return "", fmt.Errorf("the bmc platform mounts https:// and nfs:// URLs and local files, not %s", imageURL)
	}
}

// DiskMount attaches an image the BMC fetches itself, from an https:// or
// nfs:// URL, and boots the host from it. A local file goes through DiskServe.
func (b *bmcPlatform) DiskMount(ctx context.Context, image string, opts DiskOptions) error {
	if b.configErr != nil {
		return b.configErr
	}
	if !strings.Contains(image, "://") {
		return fmt.Errorf("%s is a local file, which the bmc platform serves while disk mount runs in the foreground", image)
	}
	_, err := b.mount(ctx, image, opts, nil)
	return err
}

// DiskServe serves a local image, attaches it, boots the host from it and
// serves until ctx ends. It then ejects the image and clears the boot override.
// The image goes over the BMC's NBD websocket where opts.Transport asks for it
// and the BMC offers one, and otherwise over HTTPS for the BMC to fetch.
func (b *bmcPlatform) DiskServe(ctx context.Context, image string, opts DiskServeOptions) error {
	if b.configErr != nil {
		return b.configErr
	}
	stream, err := b.routeSlot(opts.Transport)
	if err != nil {
		return err
	}
	if stream != nil {
		return b.streamMount(ctx, image, opts.DiskOptions, stream)
	}
	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("image server: %w", err)
	}
	defer listener.Close()
	base, err := b.mediaBase(opts.MediaURL, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	if err != nil {
		return err
	}
	imageURL := base.JoinPath(filepath.Base(image))
	srv, err := startImageServer(image, imageURL.Path, listener, base.Hostname(), b.progress)
	if err != nil {
		return err
	}
	defer srv.Close()
	slot, err := b.mount(ctx, imageURL.String(), opts.DiskOptions, srv)
	if err != nil {
		return err
	}
	<-ctx.Done()
	fmt.Fprintf(b.progress, "Stopping: ejecting %s.\n", slot)
	return b.DiskEject(slot)
}

// mediaBase is the URL the BMC fetches a served file under: mediaURL when set,
// otherwise https:// with the local address toward the BMC and port.
func (b *bmcPlatform) mediaBase(mediaURL, port string) (*url.URL, error) {
	if mediaURL != "" {
		u, err := url.Parse(mediaURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("--media-url %q: want an https:// URL", mediaURL)
		}
		if u.Path == "" {
			u.Path = "/"
		}
		return u, nil
	}
	if b.access.Jump != "" {
		return nil, fmt.Errorf("through a jump host benchctl cannot tell the address the BMC reaches this machine at: set --media-url or BENCHCTL_MEDIA_URL")
	}
	ip, err := b.localAddr(b.access.IP)
	if err != nil {
		return nil, fmt.Errorf("finding the local address toward the BMC: %w (set --media-url or BENCHCTL_MEDIA_URL)", err)
	}
	host := net.JoinHostPort(ip, port)
	if port == "443" {
		host = strings.TrimSuffix(host, ":443")
	}
	return &url.URL{Scheme: "https", Host: host, Path: "/"}, nil
}

// localAddrToward returns the local address the kernel picks for a packet to
// bmc. Connecting a UDP socket sends nothing.
func localAddrToward(bmc string) (string, error) {
	host := bmc
	if h, _, err := net.SplitHostPort(bmc); err == nil {
		host = h
	}
	conn, err := net.Dial("udp", net.JoinHostPort(host, "443"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// mount attaches imageURL for the host to boot: remote media on, host off,
// inserted media ejected, the image inserted, the boot override set, and the
// host on with opts.PowerOn. It then prints DiskReady and returns the ID of the
// slot that holds the image. srv is the image server for a local file, nil for
// a URL.
//
// When ctx ends, mount stops after the step it is in. When that or a failure
// comes after the insert, it ejects the slots and clears the boot override.
func (b *bmcPlatform) mount(ctx context.Context, imageURL string, opts DiskOptions, srv *imageServer) (string, error) {
	interrupted := func() error { return pause(ctx, 0) }
	stopNotice := context.AfterFunc(ctx, func() {
		fmt.Fprintln(b.progress, "Interrupted: stopping after the current step and undoing the mount. Interrupt again to exit without undoing it.")
	})
	defer stopNotice()
	protocol, err := transferProtocol(imageURL)
	if err != nil {
		return "", err
	}
	kind, err := mediaKindFor(imageURL, opts.Medium)
	if err != nil {
		return "", err
	}
	slots, err := b.insertableSlots(ctx)
	if errors.Is(err, errNoSlots) && opts.ResetBMC {
		if err := b.resetBMC(ctx); err != nil {
			return "", err
		}
		slots, err = b.insertableSlots(ctx)
	}
	if errors.Is(err, errNoSlots) {
		return "", fmt.Errorf("%w after %s: a BMC reset brought the slots back once on one board, and --reset-bmc does it", err, b.slotsTimeout)
	}
	if err == nil {
		err = checkProtocol(slots, kind, protocol)
	}
	if err == nil {
		err = interrupted()
	}
	if err != nil {
		return "", err
	}
	if err := b.setPower(ctx, PowerOff); err != nil {
		return "", err
	}
	if err := b.ejectSlots(slots); err != nil {
		return "", err
	}
	if slots, err = b.listSlots(); err != nil {
		return "", err
	}
	slot, err := pickSlot(slots, kind)
	if err == nil {
		err = interrupted()
	}
	if err != nil {
		return "", err
	}
	if err := b.insert(slot, imageURL, protocol, kind); err != nil {
		if stream, _, _ := b.streamSlot(); stream != nil {
			err = fmt.Errorf("%w\n%s", err, fmt.Sprintf(streamHint, stream.ID))
		} else if kind == kindUSB && protocol == schemas.HTTPSTransferProtocolType {
			err = fmt.Errorf("%w\n%s", err, usbHTTPSHint)
		}
		var rejected *schemas.Error // the BMC refused the POST, before any fetch
		switch {
		case errors.As(err, &rejected):
		case srv != nil && !srv.requested():
			err = fmt.Errorf("%w\n%s", err, fetchHint(imageURL, srv.port()))
		case srv == nil && hasMessage(err, msgInsertMediaError):
			err = fmt.Errorf("%w\n%s", err, fetchHint(imageURL, 0))
		}
		return "", b.undoMount(err)
	}
	holder, err := b.findInserted(imageURL)
	if err != nil {
		return "", b.undoMount(err)
	}
	enabled := schemas.ContinuousBootSourceOverrideEnabled
	if opts.BootOnce {
		enabled = schemas.OnceBootSourceOverrideEnabled
	}
	steps := []func() error{
		interrupted,
		func() error { return b.setBootOverride(ctx, kind.target, enabled) },
		interrupted,
	}
	if opts.PowerOn {
		steps = append(steps, func() error { return b.setPower(ctx, PowerOn) }, interrupted)
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return "", b.undoMount(err)
		}
	}
	fmt.Fprintln(b.progress, DiskReady)
	return holder.ID, nil
}

// undoMount powers the host off, ejects every inserted slot that has
// EjectMedia and clears the boot override after the mount failed with err, and
// returns err. With the host off, AMI takes the override PATCH it refuses while
// the host boots.
func (b *bmcPlatform) undoMount(err error) error {
	fmt.Fprintln(b.progress, "The mount stopped, powering the host off, ejecting the image and clearing the boot override...")
	undoErr := b.SetPower(PowerOff)
	if undoErr == nil {
		undoErr = b.DiskEject("")
	}
	if undoErr != nil {
		return fmt.Errorf("%w (undoing the mount also failed: %v)", err, undoErr)
	}
	return err
}

// usbHTTPSHint follows a failed insert of an https:// URL into a USB slot. An
// AMI BMC listed HTTPS in the USB slots' ActionInfo, refused an ISO there, and
// failed an .img without fetching it.
const usbHTTPSHint = "Some BMCs take USB images only over NFS or CIFS, even when they list HTTPS for the slot.\n" +
	"An ISO can go into a CD slot with --medium cd. A raw image can go over NFS: export it and mount its nfs:// URL."

// streamHint follows a failed insert on a BMC that offers the websocket route.
// An OpenBMC bmcweb called the virtual media service's Mount with an argument
// type the service did not take, which failed every insert with 500.
const streamHint = "This BMC also takes a local file over its NBD websocket in %s: --transport websocket.\n" +
	"Some OpenBMC virtual media services crash when Redfish reads that slot during the stream, see \"NBD websocket\" in docs/bmc.md."

// checkProtocol refuses protocol when every slot of kind has an InsertMedia
// ActionInfo whose TransferProtocolType values leave it out. A slot without an
// ActionInfo, or one that lists no values, may take protocol.
func checkProtocol(slots []*schemas.VirtualMedia, kind mediaKind, protocol schemas.TransferProtocolType) error {
	slot, err := pickSlot(slots, kind)
	if err != nil {
		return err
	}
	var allowed []string
	for _, other := range slots {
		if other.SupportsMediaInsert && fits(other, kind) {
			values, listed := protocolsAllowed(other)
			if !listed || slices.Contains(values, string(protocol)) {
				return nil
			}
			allowed = values
		}
	}
	return fmt.Errorf("%s takes images over %s, not %s, by its InsertMedia ActionInfo: serve the image over one of those or pick another medium with --medium",
		slot.ID, strings.Join(allowed, ", "), protocol)
}

// protocolsAllowed returns the TransferProtocolType values slot's InsertMedia
// ActionInfo lists, and whether it lists any.
func protocolsAllowed(slot *schemas.VirtualMedia) ([]string, bool) {
	info, err := slot.InsertMediaActionInfo()
	if err != nil {
		return nil, false
	}
	for _, p := range info.Parameters {
		if p.Name == "TransferProtocolType" && len(p.AllowableValues) > 0 {
			return p.AllowableValues, true
		}
	}
	return nil, false
}

// fetchHint explains the ways past a BMC that took InsertMedia for imageURL and
// failed without fetching it. port is the image server's listen port, 0 for a
// URL benchctl does not serve.
func fetchHint(imageURL string, port int) string {
	u, _ := url.Parse(imageURL)
	host := u.Hostname()
	urlPort := u.Port()
	if urlPort == "" {
		urlPort = "443"
	}
	var hint strings.Builder
	if port == 0 {
		fmt.Fprintf(&hint, "The BMC reports that it could not insert %s (port %s).\n", imageURL, urlPort)
	} else {
		fmt.Fprintf(&hint, "The image server got no request for %s (port %s) before the insert failed.\n", imageURL, urlPort)
	}
	if urlPort != "443" && port == 0 {
		fmt.Fprintf(&hint, "Some BMCs fetch only from port 443.\n")
	} else if urlPort != "443" {
		base := *u
		base.Host = strings.TrimSuffix(net.JoinHostPort(host, "443"), ":443")
		base.Path = "/"
		fmt.Fprintf(&hint, "Some BMCs fetch only from port 443. Either:\n")
		fmt.Fprintf(&hint, "  - run benchctl with --listen :443 as root, with cap_net_bind_service,\n")
		fmt.Fprintf(&hint, "    or with net.ipv4.ip_unprivileged_port_start=443,\n")
		fmt.Fprintf(&hint, "  - or redirect 443 to %d on this host (iptables/nftables REDIRECT)\n", port)
		fmt.Fprintf(&hint, "    and set --media-url %s.\n", base.String())
	}
	fmt.Fprintf(&hint, "Also check that the BMC can reach %s and that the firewall allows it.\n", host)
	fmt.Fprintf(&hint, "The BMC's message above may name another cause.")
	return hint.String()
}

// insert posts InsertMedia to slot and follows its task. HTTPS gets empty
// UserName and Password, which AMI requires, and MediaType goes along when the
// action's ActionInfo lists it.
func (b *bmcPlatform) insert(slot *schemas.VirtualMedia, imageURL string, protocol schemas.TransferProtocolType, kind mediaKind) error {
	params := &schemas.VirtualMediaInsertMediaParameters{
		Image:                imageURL,
		TransferProtocolType: &protocol,
	}
	if protocol == schemas.HTTPSTransferProtocolType {
		empty := ""
		params.UserName = &empty
		params.Password = &empty
	}
	if info, err := slot.InsertMediaActionInfo(); err == nil {
		if slices.ContainsFunc(info.Parameters, func(p schemas.ActionInfoParameter) bool { return p.Name == "MediaType" }) {
			params.MediaType = kind.mediaType
		}
	}
	fmt.Fprintf(b.progress, "Inserting %s into %s...\n", imageURL, slot.ID)
	task, err := slot.InsertMedia(params)
	if err != nil {
		return fmt.Errorf("InsertMedia on %s: %w", slot.ID, redfishError(err))
	}
	return b.followTask(task, "InsertMedia on "+slot.ID)
}

// findInserted returns the slot that holds imageURL. AMI moves media to the
// first free slot and reports the image as "//host/path", so the slot is found
// by host and path.
func (b *bmcPlatform) findInserted(imageURL string) (*schemas.VirtualMedia, error) {
	slots, err := b.listSlots()
	if err != nil {
		return nil, err
	}
	for _, slot := range slots {
		if inserted(slot) && sameImage(slot.Image, imageURL) {
			fmt.Fprintf(b.progress, "%s holds %s.\n", slot.ID, imageURL)
			return slot, nil
		}
	}
	return nil, fmt.Errorf("after InsertMedia no slot reports %s inserted (slots: %s)", imageURL, describeSlots(slots))
}

// sameImage compares two image URLs by host and path.
func sameImage(reported, want string) bool {
	r, err := url.Parse(reported)
	if err != nil {
		return false
	}
	w, err := url.Parse(want)
	if err != nil {
		return false
	}
	return r.Hostname() == w.Hostname() && r.Path == w.Path
}

// followTask polls the task an action started for up to insertTimeout until it
// completes, and fails on Exception, Killed or Cancelled with the task's
// messages. A task monitor that answers without a TaskState and not 202 has
// finished.
func (b *bmcPlatform) followTask(info *schemas.TaskMonitorInfo, what string) error {
	if info == nil {
		return nil
	}
	uri := info.TaskMonitor
	if info.Task != nil && info.Task.ODataID != "" {
		uri = info.Task.ODataID
	}
	if uri == "" {
		return nil
	}
	fmt.Fprintf(b.progress, "Waiting for %s (task %s)...\n", what, uri)
	deadline := time.Now().Add(b.insertTimeout)
	for {
		resp, err := b.media.Get(uri)
		if err != nil {
			return fmt.Errorf("%s: reading task %s: %w", what, uri, redfishError(err))
		}
		var task struct {
			TaskState string
			Messages  []schemas.Message
		}
		_ = json.NewDecoder(resp.Body).Decode(&task)
		resp.Body.Close()
		switch task.TaskState {
		case "Completed":
			return nil
		case "Exception", "Killed", "Cancelled":
			return &taskError{what: what, state: task.TaskState, messages: task.Messages}
		case "":
			if resp.StatusCode != http.StatusAccepted {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: task %s still %s after %s", what, uri, task.TaskState, b.insertTimeout)
		}
		time.Sleep(b.pollEvery)
	}
}

// taskError is a task that ended without completing.
type taskError struct {
	what     string
	state    string
	messages []schemas.Message
}

func (err *taskError) Error() string {
	var parts []string
	for _, msg := range err.messages {
		parts = append(parts, fmt.Sprintf("%s (%s)", msg.Message, msg.MessageID))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%s: task ended in %s", err.what, err.state)
	}
	return fmt.Sprintf("%s: task ended in %s: %s", err.what, err.state, strings.Join(parts, "; "))
}

// messageIDs returns the Redfish MessageIds an error carries.
func messageIDs(err error) []string {
	var ids []string
	var redfishErr *schemas.Error
	if errors.As(err, &redfishErr) {
		ids = append(ids, redfishErr.Code)
		for _, info := range redfishErr.ExtendedInfos {
			ids = append(ids, info.MessageID)
		}
	}
	var task *taskError
	if errors.As(err, &task) {
		for _, msg := range task.messages {
			ids = append(ids, msg.MessageID)
		}
	}
	return ids
}

// hasMessage reports whether err carries a MessageId ending in suffix.
func hasMessage(err error, suffix string) bool {
	return slices.ContainsFunc(messageIDs(err), func(id string) bool { return strings.HasSuffix(id, suffix) })
}

// redfishError shortens a gofish error, which holds the whole response body, to
// its status and messages.
func redfishError(err error) error {
	var redfishErr *schemas.Error
	if !errors.As(err, &redfishErr) {
		return err
	}
	var parts []string
	for _, info := range redfishErr.ExtendedInfos {
		parts = append(parts, fmt.Sprintf("%s (%s)", info.Message, info.MessageID))
	}
	if len(parts) == 0 && redfishErr.Message != "" {
		parts = append(parts, redfishErr.Message)
	}
	text := fmt.Sprintf("%d %s", redfishErr.HTTPReturnedStatusCode, http.StatusText(redfishErr.HTTPReturnedStatusCode))
	if len(parts) > 0 {
		text += ": " + strings.Join(parts, "; ")
	}
	return &shortError{text: text, err: redfishErr}
}

// shortError is a gofish error printed as its status and messages, and still
// reachable through errors.As.
type shortError struct {
	text string
	err  *schemas.Error
}

func (e *shortError) Error() string { return e.text }
func (e *shortError) Unwrap() error { return e.err }

// setBootOverride patches the system's boot override to target and enabled, in
// UEFI mode.
func (b *bmcPlatform) setBootOverride(ctx context.Context, target schemas.BootSource, enabled schemas.BootSourceOverrideEnabled) error {
	return b.patchBoot(ctx, "set the boot override", func(boot schemas.Boot) (map[string]any, error) {
		allowed := boot.AllowableBootSourceOverrideTargetValues
		if len(allowed) > 0 && !slices.Contains(allowed, target) {
			return nil, fmt.Errorf("the BMC allows no boot override to %s (it allows %v)", target, allowed)
		}
		fmt.Fprintf(b.progress, "Setting the boot override to %s, %s...\n", target, enabled)
		return map[string]any{
			"BootSourceOverrideTarget":  target,
			"BootSourceOverrideEnabled": enabled,
			"BootSourceOverrideMode":    schemas.UEFIBootSourceOverrideMode,
		}, nil
	})
}

// clearBootOverride sets BootSourceOverrideEnabled to Disabled and the target
// to None, keeping the override mode, unless no override is set. AMI requires
// the target in the PATCH and reports no override as null.
func (b *bmcPlatform) clearBootOverride() error {
	return b.patchBoot(context.Background(), "clear the boot override", func(boot schemas.Boot) (map[string]any, error) {
		if boot.BootSourceOverrideEnabled == "" || boot.BootSourceOverrideEnabled == schemas.DisabledBootSourceOverrideEnabled {
			return nil, nil
		}
		mode := boot.BootSourceOverrideMode
		if mode == "" {
			mode = schemas.UEFIBootSourceOverrideMode
		}
		fmt.Fprintln(b.progress, "Clearing the boot override...")
		return map[string]any{
			"BootSourceOverrideEnabled": schemas.DisabledBootSourceOverrideEnabled,
			"BootSourceOverrideTarget":  schemas.NoneBootSource,
			"BootSourceOverrideMode":    mode,
		}, nil
	})
}

// patchBoot reads the system and PATCHes its Boot with the fields fields
// returns, with If-Match from that read, and does nothing when fields returns
// none. While the BMC answers that the host is booting, it retries for up to
// busyTimeout or until ctx ends. It goes through PatchWithHeaders: gofish's SetBoot retries a
// failed PATCH on the settings object, which would hide the BMC's answer.
func (b *bmcPlatform) patchBoot(ctx context.Context, what string, fields func(schemas.Boot) (map[string]any, error)) error {
	deadline := time.Now().Add(b.busyTimeout)
	for {
		system, err := b.mediaSystem()
		if err != nil {
			return err
		}
		boot, err := fields(system.Boot)
		if boot == nil || err != nil {
			return err
		}
		resp, err := b.media.PatchWithHeaders(system.ODataID, map[string]any{"Boot": boot}, system.Headers())
		if err == nil {
			resp.Body.Close()
			return nil
		}
		err = fmt.Errorf("PATCH %s: %w", system.ODataID, redfishError(err))
		if !hasMessage(err, msgHostBooting) {
			return err
		}
		if time.Now().After(deadline) {
			power, _ := b.PowerState()
			return fmt.Errorf("the BMC still refuses to %s after %s while the host boots (host %s): %w", what, b.busyTimeout, power, err)
		}
		fmt.Fprintln(b.progress, "The BMC refuses the boot override while the host boots, retrying...")
		if err := pause(ctx, b.pollEvery); err != nil {
			return err
		}
	}
}

// DiskEject ejects every inserted slot that has an EjectMedia action, or only
// the one with ID only, and clears the boot override. The slots include the
// stream slot, whose eject ends a running websocket mount.
func (b *bmcPlatform) DiskEject(only string) error {
	slots, err := b.allSlots()
	if err != nil {
		return err
	}
	if only != "" {
		i := slices.IndexFunc(slots, func(slot *schemas.VirtualMedia) bool { return slot.ID == only })
		if i < 0 {
			return fmt.Errorf("the BMC has no virtual media slot %q (it has %s)", only, describeSlots(slots))
		}
		if inserted(slots[i]) && !slots[i].SupportsMediaEject {
			return fmt.Errorf("slot %s has no EjectMedia action, so benchctl leaves it alone", only)
		}
		slots = slots[i : i+1]
	}
	return errors.Join(b.ejectSlots(slots), b.clearBootOverride())
}

// ejectSlots posts EjectMedia to every inserted slot among slots that has the
// action. Slots without it belong to someone else's session.
func (b *bmcPlatform) ejectSlots(slots []*schemas.VirtualMedia) error {
	for _, slot := range slots {
		if !inserted(slot) || !slot.SupportsMediaEject {
			continue
		}
		if slot.Image == "" {
			fmt.Fprintf(b.progress, "Ejecting %s...\n", slot.ID)
		} else {
			fmt.Fprintf(b.progress, "Ejecting %s from %s...\n", slot.Image, slot.ID)
		}
		task, err := slot.EjectMedia()
		if err != nil {
			return fmt.Errorf("EjectMedia on %s: %w", slot.ID, redfishError(err))
		}
		if err := b.followTask(task, "EjectMedia on "+slot.ID); err != nil {
			return err
		}
	}
	return nil
}

// DiskStatus prints the slots, the stream slot included, the boot override and,
// on AMI, the remote media status.
func (b *bmcPlatform) DiskStatus() error {
	slots, err := b.allSlots()
	if err != nil {
		return err
	}
	table := tabwriter.NewWriter(b.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "SLOT\tTYPES\tINSERTED\tWRITE-PROTECTED\tACTIONS\tIMAGE")
	for _, slot := range slots {
		var actions []string
		if slot.SupportsMediaInsert {
			actions = append(actions, "insert")
		}
		if slot.SupportsMediaEject {
			actions = append(actions, "eject")
		}
		writeProtected := "-"
		if slot.WriteProtected != nil {
			writeProtected = fmt.Sprint(*slot.WriteProtected)
		}
		fmt.Fprintf(table, "%s\t%s\t%v\t%s\t%s\t%s\n", slot.ID, orDash(mediaTypes(slot, ",")), inserted(slot), writeProtected, orDash(strings.Join(actions, ",")), orDash(slot.Image))
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if len(slots) == 0 {
		fmt.Fprintln(b.out, "The BMC offers no virtual media slots.")
	}
	system, managers, err := b.mediaManagers()
	if err != nil {
		return err
	}
	boot := system.Boot
	fmt.Fprintf(b.out, "Boot override: %s, %s, %s\n", orDash(string(boot.BootSourceOverrideTarget)), orDash(string(boot.BootSourceOverrideEnabled)), orDash(string(boot.BootSourceOverrideMode)))
	for _, manager := range managers {
		state, err := b.readManager(manager.ODataID)
		if err != nil {
			return err
		}
		if status := state.Oem.Ami.VirtualMedia.RMediaStatus; status != "" {
			fmt.Fprintf(b.out, "Remote media on %s: %s\n", manager.ID, status)
		}
	}
	return nil
}

// orDash returns s, or "-" when it is empty.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// resetBMC restarts the BMC with Manager.Reset ForceRestart and waits up to
// bmcResetTimeout, or until ctx ends, for Redfish to go away and answer again.
func (b *bmcPlatform) resetBMC(ctx context.Context) error {
	_, managers, err := b.mediaManagers()
	if err != nil {
		return err
	}
	if len(managers) == 0 {
		return fmt.Errorf("the BMC lists no manager to reset")
	}
	manager := managers[0]
	state, err := b.readManager(manager.ODataID)
	if err != nil {
		return err
	}
	reset := state.Actions.Reset
	if len(reset.Allowed) > 0 && !slices.Contains(reset.Allowed, "ForceRestart") {
		return fmt.Errorf("the BMC allows no ForceRestart for Manager.Reset (it allows %s)", strings.Join(reset.Allowed, ", "))
	}
	target := reset.Target
	if target == "" {
		target = manager.ODataID + "/Actions/Manager.Reset"
	}
	fmt.Fprintf(b.progress, "Resetting the BMC with Manager.Reset ForceRestart. Consoles attached through the BMC drop.\n")
	resp, err := b.media.PostWithHeaders(target, map[string]string{"ResetType": "ForceRestart"}, manager.Headers())
	if err != nil {
		return fmt.Errorf("POST %s: %w", target, redfishError(err))
	}
	resp.Body.Close()
	b.media = nil
	deadline := time.Now().Add(b.bmcResetTimeout)
	down := false
	for {
		var root struct{}
		err := b.redfish.Get("/redfish/v1", &root)
		switch {
		case err != nil && !down:
			fmt.Fprintln(b.progress, "The BMC is restarting...")
			down = true
		case err == nil && down:
			fmt.Fprintln(b.progress, "The BMC answers again.")
			return nil
		}
		if time.Now().After(deadline) {
			if down {
				return fmt.Errorf("the BMC does not answer %s after the reset: %w", b.bmcResetTimeout, err)
			}
			return fmt.Errorf("the BMC still answered %s after the reset, so it did not restart", b.bmcResetTimeout)
		}
		if err := pause(ctx, b.pollEvery); err != nil {
			return err
		}
	}
}
