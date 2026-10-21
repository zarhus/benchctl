// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// mediaBMC is a Redfish service with virtual media in one of two flavours. With
// ami set it answers as the AMI MegaRAC bench BMC did: remote media to enable,
// no override reported as null, If-Match on PATCH, BootSourceOverrideTarget and
// BootSourceOverrideMode required, media moved to the first free slot and
// reported as "//host/path", images fetched as "//path", and tasks for the
// media actions.
// Without it, it answers as an OpenBMC build with remote media does: a proxy
// slot without actions, USB slots that list both media types, and 204 answers.
type mediaBMC struct {
	mu sync.Mutex

	ami     bool
	system  string // ComputerSystem ID
	manager string // Manager ID
	slots   []*fakeSlot

	powerState   string
	rmedia       string   // AMI RMediaStatus, empty for none
	emptyPolls   int      // collection reads that stay empty after remote media is on
	untilReset   bool     // the collection stays empty until a manager reset
	busy         int      // boot PATCHes answered 503 while the host boots
	fetch        bool     // InsertMedia fetches the image as a BMC does
	failInsert   bool     // the insert task ends in Exception
	rejectInsert bool     // InsertMedia answers 400 before any fetch
	protocols    []string // TransferProtocolType values the AMI ActionInfo lists, nil for none
	misreport    bool     // a slot reports an inserted image under another path
	failPowerOn  bool     // ComputerSystem.Reset to On fails
	failEject    bool     // EjectMedia fails
	busyWhileOn  bool     // boot PATCHes answered 503 whenever the host is on
	onInsert     func()   // called on every InsertMedia
	onPowerOn    func()   // called on every ComputerSystem.Reset to On
	down         int      // service root reads that fail after a manager reset
	etag         int
	boot         map[string]any
	resets       []string
	mgrResets    int
	patches      []map[string]any
	ifMatch      []string
	inserts      []map[string]any // InsertMedia bodies, with "slot" set to the addressed slot
	ejects       []string
	tasks        map[string]map[string]any
	fetched      []string // requests made to the image server, as "METHOD range"
	httpsClient  *http.Client
}

// fakeSlot is one virtual media slot.
type fakeSlot struct {
	id            string
	types         []string
	insert, eject bool
	inserted      bool
	image         string
}

func amiMediaBMC() *mediaBMC {
	return &mediaBMC{
		ami:        true,
		system:     "Self",
		manager:    "Self",
		powerState: "On",
		rmedia:     "Enabled",
		boot:       map[string]any{},
		slots: []*fakeSlot{
			{id: "CD1", types: []string{"CD"}, insert: true, eject: true},
			{id: "CD2", types: []string{"CD"}, insert: true, eject: true},
			{id: "USBStick1", types: []string{"USBStick"}, insert: true, eject: true},
			{id: "USBStick2", types: []string{"USBStick"}, insert: true, eject: true},
		},
	}
}

func openBMCMediaBMC() *mediaBMC {
	both := []string{"CD", "USBStick"}
	return &mediaBMC{
		system:     "System_0",
		manager:    "BMC_0",
		powerState: "On",
		boot:       map[string]any{"BootSourceOverrideEnabled": "Disabled", "BootSourceOverrideTarget": "None", "BootSourceOverrideMode": "UEFI"},
		slots: []*fakeSlot{
			{id: "Slot_0", types: both, inserted: true, image: "proxy"},
			{id: "USB1", types: both, insert: true, eject: true},
			{id: "USB2", types: both, insert: true, eject: true},
		},
	}
}

func (bmc *mediaBMC) systemPath() string  { return "/redfish/v1/Systems/" + bmc.system }
func (bmc *mediaBMC) managerPath() string { return "/redfish/v1/Managers/" + bmc.manager }
func (bmc *mediaBMC) mediaPath() string   { return bmc.managerPath() + "/VirtualMedia" }

func (bmc *mediaBMC) etagValue() string { return fmt.Sprintf(`W/"%d"`, bmc.etag) }

// redfishFail answers status with a Redfish error naming id.
func redfishFail(w http.ResponseWriter, status int, id string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code":    id,
		"message": id,
		"@Message.ExtendedInfo": []map[string]any{
			{"MessageId": id, "Message": "message for " + id},
		},
	}})
}

func (bmc *mediaBMC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bmc.mu.Lock()
	defer bmc.mu.Unlock()
	// gofish reads the service root as "/redfish/v1/", before it
	// authenticates, which DMTF allows.
	p := strings.TrimSuffix(r.URL.Path, "/")
	if user, password, ok := r.BasicAuth(); (!ok || user != "root" || password != "0penBmc") && p != "/redfish/v1" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var body map[string]any
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case r.Method == http.MethodGet && p == "/redfish/v1":
		if bmc.down > 0 {
			bmc.down--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		reply(map[string]any{
			"@odata.id": "/redfish/v1",
			"Systems":   map[string]string{"@odata.id": "/redfish/v1/Systems"},
			"Managers":  map[string]string{"@odata.id": "/redfish/v1/Managers"},
		})
	case r.Method == http.MethodGet && p == "/redfish/v1/Systems":
		reply(map[string]any{"Members": []map[string]string{{"@odata.id": bmc.systemPath()}}})
	case r.Method == http.MethodGet && p == "/redfish/v1/Managers":
		reply(map[string]any{"Members": []map[string]string{{"@odata.id": bmc.managerPath()}}})
	case r.Method == http.MethodGet && p == bmc.systemPath():
		boot := map[string]any{"BootSourceOverrideTarget@Redfish.AllowableValues": []string{"None", "Pxe", "Hdd", "Cd", "Usb", "BiosSetup"}}
		for k, v := range bmc.boot {
			boot[k] = v
		}
		reply(map[string]any{
			"@odata.id":   p,
			"@odata.etag": bmc.etagValue(),
			"Id":          bmc.system,
			"PowerState":  bmc.powerState,
			"Boot":        boot,
			"Actions":     map[string]any{"#ComputerSystem.Reset": map[string]any{"target": p + "/Actions/ComputerSystem.Reset"}},
			"Links":       map[string]any{"ManagedBy": []map[string]string{{"@odata.id": bmc.managerPath()}}},
		})
	case r.Method == http.MethodPost && p == bmc.systemPath()+"/Actions/ComputerSystem.Reset":
		resetType, _ := body["ResetType"].(string)
		bmc.resets = append(bmc.resets, resetType)
		if resetType != "ForceOff" {
			if bmc.failPowerOn {
				redfishFail(w, http.StatusInternalServerError, "Base.1.12.InternalError")
				return
			}
			if bmc.onPowerOn != nil {
				bmc.onPowerOn()
			}
		}
		if resetType == "ForceOff" {
			bmc.powerState = "Off"
		} else {
			bmc.powerState = "On"
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && p == bmc.systemPath():
		bmc.ifMatch = append(bmc.ifMatch, r.Header.Get("If-Match"))
		boot, _ := body["Boot"].(map[string]any)
		if bmc.ami {
			if r.Header.Get("If-Match") == "" {
				redfishFail(w, http.StatusPreconditionRequired, "Ami.1.0.PreconditionHeaderMissing")
				return
			}
			if r.Header.Get("If-Match") != bmc.etagValue() {
				redfishFail(w, http.StatusPreconditionFailed, "Base.1.12.PreconditionFailed")
				return
			}
			if bmc.busyWhileOn && bmc.powerState == "On" {
				redfishFail(w, http.StatusServiceUnavailable, "Ami.1.0.ServiceTemporarilyUnavailableDueToHostBooting")
				return
			}
			if bmc.busy > 0 {
				bmc.busy--
				redfishFail(w, http.StatusServiceUnavailable, "Ami.1.0.ServiceTemporarilyUnavailableDueToHostBooting")
				return
			}
			for _, required := range []string{"BootSourceOverrideTarget", "BootSourceOverrideMode"} {
				if _, ok := boot[required]; !ok {
					redfishFail(w, http.StatusBadRequest, "Base.1.12.PropertyMissing")
					return
				}
			}
		}
		bmc.patches = append(bmc.patches, boot)
		for k, v := range boot {
			bmc.boot[k] = v
		}
		bmc.etag++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && p == bmc.managerPath():
		manager := map[string]any{
			"@odata.id":    p,
			"Id":           bmc.manager,
			"VirtualMedia": map[string]string{"@odata.id": bmc.mediaPath()},
		}
		actions := map[string]any{"#Manager.Reset": map[string]any{
			"target":                            p + "/Actions/Manager.Reset",
			"ResetType@Redfish.AllowableValues": []string{"ForceRestart"},
		}}
		if bmc.ami {
			manager["Oem"] = map[string]any{"Ami": map[string]any{"VirtualMedia": map[string]any{"RMediaStatus": bmc.rmedia}}}
			actions["Oem"] = map[string]any{"#AMIVirtualMedia.EnableRMedia": map[string]any{"target": p + "/Actions/Oem/AMIVirtualMedia.EnableRMedia"}}
		}
		manager["Actions"] = actions
		reply(manager)
	case r.Method == http.MethodPost && p == bmc.managerPath()+"/Actions/Oem/AMIVirtualMedia.EnableRMedia":
		bmc.rmedia = "Enabled"
		redfishFail(w, http.StatusOK, "Ami.1.0.DelayInActionCompletion")
	case r.Method == http.MethodPost && p == bmc.managerPath()+"/Actions/Manager.Reset":
		bmc.mgrResets++
		bmc.down = 2
		bmc.untilReset = false
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && p == bmc.mediaPath():
		var members []map[string]string
		switch {
		case bmc.rmedia == "Disabled" || bmc.untilReset:
		case bmc.emptyPolls > 0:
			bmc.emptyPolls--
		default:
			for _, slot := range bmc.slots {
				members = append(members, map[string]string{"@odata.id": bmc.mediaPath() + "/" + slot.id})
			}
		}
		reply(map[string]any{"Members": members, "Members@odata.count": len(members)})
	case strings.HasPrefix(p, bmc.mediaPath()+"/"):
		bmc.serveSlot(w, r, strings.TrimPrefix(p, bmc.mediaPath()+"/"), body)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/redfish/v1/TaskService/Tasks/"):
		task, ok := bmc.tasks[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		reply(task)
	default:
		http.NotFound(w, r)
	}
}

// serveSlot answers the requests below one slot.
func (bmc *mediaBMC) serveSlot(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	id, action, _ := strings.Cut(rest, "/")
	slot := bmc.slot(id)
	if slot == nil {
		http.NotFound(w, r)
		return
	}
	path := bmc.mediaPath() + "/" + id
	switch {
	case r.Method == http.MethodGet && action == "":
		actions := map[string]any{}
		if slot.insert {
			insert := map[string]any{"target": path + "/Actions/VirtualMedia.InsertMedia"}
			if bmc.ami {
				insert["@Redfish.ActionInfo"] = path + "/InsertMediaActionInfo"
			}
			actions["#VirtualMedia.InsertMedia"] = insert
		}
		if slot.eject {
			actions["#VirtualMedia.EjectMedia"] = map[string]any{"target": path + "/Actions/VirtualMedia.EjectMedia"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"@odata.id":      path,
			"@odata.etag":    bmc.etagValue(),
			"Id":             id,
			"MediaTypes":     slot.types,
			"Inserted":       slot.inserted,
			"Image":          slot.image,
			"WriteProtected": true,
			"Actions":        actions,
		})
	case r.Method == http.MethodGet && action == "InsertMediaActionInfo":
		_ = json.NewEncoder(w).Encode(map[string]any{"Parameters": []map[string]any{
			{"Name": "Image", "Required": true},
			{"Name": "TransferProtocolType", "Required": true, "AllowableValues": bmc.protocols},
			{"Name": "UserName"}, {"Name": "Password"}, {"Name": "MediaType"},
		}})
	case r.Method == http.MethodPost && action == "Actions/VirtualMedia.InsertMedia":
		body["slot"] = id
		bmc.inserts = append(bmc.inserts, body)
		if bmc.rejectInsert {
			redfishFail(w, http.StatusBadRequest, "Base.1.12.ActionParameterValueFormatError")
			return
		}
		if bmc.onInsert != nil {
			bmc.onInsert()
		}
		image, _ := body["Image"].(string)
		if bmc.ami {
			_, hasUser := body["UserName"]
			_, hasPassword := body["Password"]
			if body["TransferProtocolType"] == nil || body["TransferProtocolType"] == "HTTPS" && (!hasUser || !hasPassword) {
				redfishFail(w, http.StatusBadRequest, "Base.1.12.ActionParameterMissing")
				return
			}
			// AMI moves the media to the first free slot of the same kind.
			for _, other := range bmc.slots {
				if !other.inserted && slices.Equal(other.types, slot.types) {
					slot = other
					break
				}
			}
		}
		failed := bmc.failInsert
		if bmc.fetch && !failed {
			failed = !bmc.fetchImage(image)
		}
		if !failed {
			slot.inserted = true
			slot.image = image
			if bmc.ami {
				u, _ := url.Parse(image)
				slot.image = "//" + u.Hostname() + u.Path
			}
			if bmc.misreport {
				slot.image += ".moved"
			}
			bmc.etag++
		}
		if !bmc.ami {
			if failed {
				redfishFail(w, http.StatusInternalServerError, "Base.1.12.InternalError")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		state, messages := "Completed", []map[string]string{}
		if failed {
			state = "Exception"
			messages = append(messages, map[string]string{"MessageId": "SyncAgent.1.0.InsertMediaError", "Message": "Insert media failed."})
		}
		bmc.startTask(w, map[string]any{"TaskState": state, "Messages": messages})
	case r.Method == http.MethodPost && action == "Actions/VirtualMedia.EjectMedia":
		if bmc.failEject {
			redfishFail(w, http.StatusInternalServerError, "Base.1.12.InternalError")
			return
		}
		bmc.ejects = append(bmc.ejects, id)
		slot.inserted = false
		slot.image = ""
		bmc.etag++
		if bmc.ami {
			bmc.startTask(w, map[string]any{"TaskState": "Completed"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && action == "":
		// A PATCH of Inserted is what gofish sends to a slot without
		// EjectMedia. It would end the session that holds the slot.
		bmc.ejects = append(bmc.ejects, id+" by PATCH")
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// startTask answers 202 with a new task that reads as final.
func (bmc *mediaBMC) startTask(w http.ResponseWriter, final map[string]any) {
	if bmc.tasks == nil {
		bmc.tasks = map[string]map[string]any{}
	}
	path := fmt.Sprintf("/redfish/v1/TaskService/Tasks/%d", len(bmc.tasks)+1)
	final["@odata.id"] = path
	bmc.tasks[path] = final
	w.Header().Set("Location", path+"/Monitor")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"@odata.id": path, "TaskState": "New"})
}

// fetchImage reads the image as the bench BMC did: HEAD, HEAD with a range,
// then a ranged GET, with the path after a second slash.
func (bmc *mediaBMC) fetchImage(image string) bool {
	u, err := url.Parse(image)
	if err != nil {
		return false
	}
	u.Path = "/" + u.Path
	image = u.String()
	for _, req := range []struct{ method, rng string }{{http.MethodHead, ""}, {http.MethodHead, "bytes=0-7"}, {http.MethodGet, "bytes=2-5"}} {
		r, err := http.NewRequest(req.method, image, nil)
		if err != nil {
			return false
		}
		if req.rng != "" {
			r.Header.Set("Range", req.rng)
		}
		resp, err := bmc.httpsClient.Do(r)
		if err != nil {
			return false
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		bmc.fetched = append(bmc.fetched, fmt.Sprintf("%s %s %d %q", req.method, req.rng, resp.StatusCode, data))
		if resp.StatusCode/100 != 2 {
			return false
		}
	}
	return true
}

// newMediaTestBMC starts bmc over TLS and returns a driver that reaches it.
func newMediaTestBMC(t *testing.T, bmc *mediaBMC) (*bmcPlatform, *syncWriter, *bytes.Buffer) {
	t.Helper()
	bmc.httpsClient = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	driver, progress := newTestBMC(t, bmc, BMCAccess{}, &fakeRunner{})
	driver.slotsTimeout = time.Second
	driver.busyTimeout = time.Second
	driver.bmcResetTimeout = time.Second
	driver.localAddr = func(string) (string, error) { return "127.0.0.1", nil }
	out := &bytes.Buffer{}
	driver.out = out
	return driver, progress, out
}

func (bmc *mediaBMC) slot(id string) *fakeSlot {
	for _, slot := range bmc.slots {
		if slot.id == id {
			return slot
		}
	}
	return nil
}

func TestBMCDiskMountOnAMIEnablesRemoteMediaAndBootsTheCD(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.rmedia = "Disabled"
	bmc.emptyPolls = 2
	// Media a killed run left behind.
	bmc.slots[2].inserted = true
	bmc.slots[2].image = "//10.0.0.9/old.img"
	driver, progress, _ := newMediaTestBMC(t, bmc)

	if err := driver.DiskMount(context.Background(), "https://10.0.0.9/images/debian.iso", DiskOptions{}); err != nil {
		t.Fatalf("%v\n%s", err, progress)
	}
	if bmc.rmedia != "Enabled" {
		t.Errorf("RMediaStatus = %q, want Enabled", bmc.rmedia)
	}
	if want := []string{"ForceOff"}; !slices.Equal(bmc.resets, want) {
		t.Errorf("resets = %q, want %q: the host stays off without PowerOn", bmc.resets, want)
	}
	if want := []string{"USBStick1"}; !slices.Equal(bmc.ejects, want) {
		t.Errorf("ejects = %q, want %q", bmc.ejects, want)
	}
	if len(bmc.inserts) != 1 {
		t.Fatalf("inserts = %v, want one", bmc.inserts)
	}
	insert := bmc.inserts[0]
	if insert["slot"] != "CD1" || insert["Image"] != "https://10.0.0.9/images/debian.iso" ||
		insert["TransferProtocolType"] != "HTTPS" || insert["UserName"] != "" || insert["Password"] != "" || insert["MediaType"] != "CD" {
		t.Errorf("InsertMedia body = %v, want CD1, the URL, HTTPS, empty credentials and MediaType CD", insert)
	}
	if cd := bmc.slot("CD1"); !cd.inserted || cd.image != "//10.0.0.9/images/debian.iso" {
		t.Errorf("CD1 = %+v, want the image inserted", cd)
	}
	wantBoot := map[string]any{"BootSourceOverrideTarget": "Cd", "BootSourceOverrideEnabled": "Continuous", "BootSourceOverrideMode": "UEFI"}
	if len(bmc.patches) != 1 || fmt.Sprint(bmc.patches[0]) != fmt.Sprint(wantBoot) {
		t.Errorf("boot PATCHes = %v, want %v", bmc.patches, wantBoot)
	}
	if bmc.powerState != "Off" {
		t.Errorf("power = %s, want Off", bmc.powerState)
	}
	if !strings.HasSuffix(progress.String(), DiskReady+"\n") {
		t.Errorf("progress = %q, want it to end with %s", progress, DiskReady)
	}
}

func TestBMCDiskMountOnOpenBMCUsesAUSBSlotAndLeavesTheProxySlotAlone(t *testing.T) {
	bmc := openBMCMediaBMC()
	driver, progress, _ := newMediaTestBMC(t, bmc)

	if err := driver.DiskMount(context.Background(), "https://10.0.0.9/disk.img", DiskOptions{BootOnce: true, PowerOn: true}); err != nil {
		t.Fatalf("%v\n%s", err, progress)
	}
	if want := []string{"ForceOff", "On"}; !slices.Equal(bmc.resets, want) || bmc.powerState != "On" {
		t.Errorf("resets = %q, power %s, want %q and On with PowerOn", bmc.resets, bmc.powerState, want)
	}
	if len(bmc.ejects) != 0 {
		t.Errorf("ejects = %q, want none: Slot_0 has no EjectMedia", bmc.ejects)
	}
	if len(bmc.inserts) != 1 || bmc.inserts[0]["slot"] != "USB1" {
		t.Fatalf("inserts = %v, want one into USB1", bmc.inserts)
	}
	if _, ok := bmc.inserts[0]["MediaType"]; ok {
		t.Errorf("InsertMedia body = %v, want no MediaType without an ActionInfo that lists it", bmc.inserts[0])
	}
	if got := bmc.boot["BootSourceOverrideTarget"]; got != "Usb" {
		t.Errorf("boot target = %v, want Usb", got)
	}
	if got := bmc.boot["BootSourceOverrideEnabled"]; got != "Once" {
		t.Errorf("boot override = %v, want Once", got)
	}
}

func TestBMCDiskMountRetriesTheOverrideWhileTheHostBoots(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.busy = 2
	driver, progress, _ := newMediaTestBMC(t, bmc)

	if err := driver.DiskMount(context.Background(), "https://10.0.0.9/debian.iso", DiskOptions{Medium: DiskCD}); err != nil {
		t.Fatalf("%v\n%s", err, progress)
	}
	if len(bmc.ifMatch) != 3 {
		t.Fatalf("boot PATCHes = %d, want 3", len(bmc.ifMatch))
	}
	if bmc.ifMatch[2] == "" || len(bmc.patches) != 1 {
		t.Errorf("If-Match = %q, patches = %v, want the third PATCH accepted with an etag", bmc.ifMatch, bmc.patches)
	}

	bmc.busy = 1000
	bmc.boot["BootSourceOverrideEnabled"] = "Disabled"
	err := driver.DiskMount(context.Background(), "https://10.0.0.9/debian.iso", DiskOptions{})
	if err == nil || !strings.Contains(err.Error(), "while the host boots") {
		t.Fatalf("DiskMount with the BMC busy = %v, want the override refused while the host boots", err)
	}
	if slices.ContainsFunc(bmc.slots, func(slot *fakeSlot) bool { return slot.inserted }) {
		t.Error("a failed mount left media inserted")
	}
}

func TestBMCDiskMountReportsAFailedInsertTask(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.failInsert = true
	driver, _, _ := newMediaTestBMC(t, bmc)

	err := driver.DiskMount(context.Background(), "https://10.0.0.9/debian.iso", DiskOptions{})
	if err == nil || !strings.Contains(err.Error(), "Exception") || !strings.Contains(err.Error(), "SyncAgent.1.0.InsertMediaError") {
		t.Fatalf("DiskMount = %v, want the task's Exception and message", err)
	}
	if !strings.Contains(err.Error(), "The BMC reports that it could not insert https://10.0.0.9/debian.iso (port 443)") {
		t.Errorf("DiskMount = %v, want the fetch hint for InsertMediaError", err)
	}
	if len(bmc.patches) != 0 || slices.Contains(bmc.resets, "On") {
		t.Errorf("patches = %v, resets = %q, want no override and no power on after a failed insert", bmc.patches, bmc.resets)
	}
}

func TestBMCDiskMountWithoutSlotsSuggestsAndDoesTheBMCReset(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.untilReset = true
	driver, _, _ := newMediaTestBMC(t, bmc)
	driver.slotsTimeout = 50 * time.Millisecond

	err := driver.DiskMount(context.Background(), "https://10.0.0.9/debian.iso", DiskOptions{})
	if !errors.Is(err, errNoSlots) || !strings.Contains(err.Error(), "--reset-bmc") {
		t.Fatalf("DiskMount = %v, want errNoSlots naming --reset-bmc", err)
	}
	if bmc.mgrResets != 0 {
		t.Errorf("manager resets = %d without --reset-bmc, want 0", bmc.mgrResets)
	}

	driver.bmcResetTimeout = 5 * time.Second
	if err := driver.DiskMount(context.Background(), "https://10.0.0.9/debian.iso", DiskOptions{ResetBMC: true}); err != nil {
		t.Fatal(err)
	}
	if bmc.mgrResets != 1 || bmc.down != 0 {
		t.Errorf("manager resets = %d, down = %d, want one reset waited out", bmc.mgrResets, bmc.down)
	}
	if !bmc.slot("CD1").inserted {
		t.Error("CD1 empty after the mount that followed the BMC reset")
	}
}

func TestBMCDiskMountRejectsWhatItCannotMount(t *testing.T) {
	driver, _, _ := newMediaTestBMC(t, amiMediaBMC())
	for image, opts := range map[string]DiskOptions{
		"debian.iso":                 {},
		"ftp://10.0.0.9/debian.iso":  {},
		"https://10.0.0.9/disk.img#": {Medium: DiskVirtio},
	} {
		if err := driver.DiskMount(context.Background(), image, opts); err == nil {
			t.Errorf("DiskMount(%q, %+v): want an error, got nil", image, opts)
		}
	}
}

func TestBMCDiskEjectEjectsInsertedSlotsAndClearsTheOverride(t *testing.T) {
	bmc := openBMCMediaBMC()
	bmc.slots[2].inserted = true
	bmc.slots[2].image = "https://10.0.0.9/disk.img"
	bmc.boot = map[string]any{"BootSourceOverrideEnabled": "Continuous", "BootSourceOverrideTarget": "Usb", "BootSourceOverrideMode": "UEFI"}
	driver, _, _ := newMediaTestBMC(t, bmc)

	if err := driver.DiskEject(""); err != nil {
		t.Fatal(err)
	}
	if want := []string{"USB2"}; !slices.Equal(bmc.ejects, want) {
		t.Errorf("ejects = %q, want %q", bmc.ejects, want)
	}
	if !bmc.slot("Slot_0").inserted {
		t.Error("Slot_0 ejected, want the proxy slot left alone")
	}
	if got := bmc.boot["BootSourceOverrideEnabled"]; got != "Disabled" {
		t.Errorf("boot override = %v, want Disabled", got)
	}

	if err := driver.DiskEject("Slot_0"); err == nil || !strings.Contains(err.Error(), "no EjectMedia") {
		t.Errorf("DiskEject(Slot_0) = %v, want a refusal", err)
	}
	if err := driver.DiskEject("CD9"); err == nil || !strings.Contains(err.Error(), "no virtual media slot") {
		t.Errorf("DiskEject(CD9) = %v, want an unknown slot", err)
	}
	patches := len(bmc.patches)
	if err := driver.DiskEject("USB1"); err != nil {
		t.Fatal(err)
	}
	if len(bmc.patches) != patches {
		t.Errorf("boot PATCHes = %v, want none for an override already Disabled", bmc.patches[patches:])
	}
}

func TestBMCDiskMountUndoesStepsAfterTheInsert(t *testing.T) {
	for name, setup := range map[string]func(*mediaBMC){
		"slot not found": func(bmc *mediaBMC) { bmc.misreport = true },
		"power on fails": func(bmc *mediaBMC) { bmc.failPowerOn = true },
	} {
		t.Run(name, func(t *testing.T) {
			bmc := amiMediaBMC()
			// An override a killed run left behind.
			bmc.boot["BootSourceOverrideEnabled"] = "Continuous"
			setup(bmc)
			driver, progress, _ := newMediaTestBMC(t, bmc)

			if err := driver.DiskMount(context.Background(), "https://10.0.0.9/debian.iso", DiskOptions{PowerOn: true}); err == nil {
				t.Fatal("DiskMount: want an error, got nil")
			}
			if slices.ContainsFunc(bmc.slots, func(slot *fakeSlot) bool { return slot.inserted }) {
				t.Errorf("a failed mount left media inserted\n%s", progress)
			}
			if got := bmc.boot["BootSourceOverrideEnabled"]; got != "Disabled" {
				t.Errorf("boot override = %v after a failed mount, want Disabled", got)
			}
			if strings.Contains(progress.String(), DiskReady) {
				t.Errorf("progress = %q, want no %s", progress, DiskReady)
			}
		})
	}
}

func TestBMCDiskEjectLeavesANullOverrideAlone(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.powerState = "Off"
	driver, _, _ := newMediaTestBMC(t, bmc)

	if err := driver.DiskEject(""); err != nil {
		t.Fatal(err)
	}
	if len(bmc.ifMatch) != 0 {
		t.Errorf("boot PATCHes = %d, want none while no override is set", len(bmc.ifMatch))
	}
}

func TestBMCDiskMountStopsWaitingOnASignal(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.untilReset = true
	driver, progress, _ := newMediaTestBMC(t, bmc)
	driver.slotsTimeout = time.Minute
	driver.pollEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	err := driver.DiskMount(ctx, "https://10.0.0.9/debian.iso", DiskOptions{})
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("DiskMount = %v, want errInterrupted", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("DiskMount took %s after the signal, want it to stop waiting for slots", elapsed)
	}
	waitFor(t, func() bool {
		return strings.Contains(progress.String(), "Interrupted: stopping after the current step")
	}, "the interrupt notice")
}

func TestBMCDiskEjectClearsTheOverrideWhenAnEjectFails(t *testing.T) {
	bmc := openBMCMediaBMC()
	bmc.slots[1].inserted = true
	bmc.slots[1].image = "https://10.0.0.9/disk.img"
	bmc.boot["BootSourceOverrideEnabled"] = "Continuous"
	bmc.failEject = true
	driver, _, _ := newMediaTestBMC(t, bmc)

	if err := driver.DiskEject(""); err == nil || !strings.Contains(err.Error(), "EjectMedia on USB1") {
		t.Errorf("DiskEject = %v, want the failed EjectMedia", err)
	}
	if got := bmc.boot["BootSourceOverrideEnabled"]; got != "Disabled" {
		t.Errorf("boot override = %v, want Disabled", got)
	}
}

func TestBMCDiskServeStopsAndUndoesTheMountOnASignal(t *testing.T) {
	bmc := amiMediaBMC()
	driver, progress, _ := newMediaTestBMC(t, bmc)
	image := filepath.Join(t.TempDir(), "debian.iso")
	if err := os.WriteFile(image, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bmc.onInsert = cancel

	err := driver.DiskServe(ctx, image, DiskServeOptions{Listen: "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("DiskServe = %v, want interrupted", err)
	}
	if slices.ContainsFunc(bmc.slots, func(slot *fakeSlot) bool { return slot.inserted }) {
		t.Error("an interrupted mount left media inserted")
	}
	if len(bmc.patches) != 0 || slices.Contains(bmc.resets, "On") {
		t.Errorf("patches = %v, resets = %q, want no override and no power on after the signal", bmc.patches, bmc.resets)
	}
	if strings.Contains(progress.String(), DiskReady) {
		t.Errorf("progress = %q, want no %s", progress, DiskReady)
	}
}

func TestBMCDiskMountFromAURLUndoesItOnASignalAfterPowerOn(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.busyWhileOn = true
	driver, progress, _ := newMediaTestBMC(t, bmc)
	ctx, cancel := context.WithCancel(context.Background())
	bmc.onPowerOn = cancel

	err := driver.DiskMount(ctx, "https://10.0.0.9/debian.iso", DiskOptions{PowerOn: true})
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("DiskMount = %v, want interrupted\n%s", err, progress)
	}
	if slices.ContainsFunc(bmc.slots, func(slot *fakeSlot) bool { return slot.inserted }) {
		t.Error("an interrupted mount left media inserted")
	}
	if bmc.powerState != "Off" || bmc.boot["BootSourceOverrideEnabled"] != "Disabled" {
		t.Errorf("power %s, boot override %v, want the host off and the override cleared", bmc.powerState, bmc.boot["BootSourceOverrideEnabled"])
	}
}

func TestBMCDiskServeServesUnderTheMediaURLPath(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.fetch = true
	driver, progress, _ := newMediaTestBMC(t, bmc)
	image := filepath.Join(t.TempDir(), "debian.iso")
	if err := os.WriteFile(image, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := free.Addr().String()
	free.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- driver.DiskServe(ctx, image, DiskServeOptions{Listen: listen, MediaURL: "https://" + listen + "/images/"})
	}()
	waitFor(t, func() bool { return strings.Contains(progress.String(), DiskReady) }, DiskReady)
	bmc.mu.Lock()
	imageURL, _ := bmc.inserts[0]["Image"].(string)
	bmc.mu.Unlock()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("%v\n%s", err, progress)
	}
	if want := "https://" + listen + "/images/debian.iso"; imageURL != want {
		t.Errorf("image URL = %q, want %q", imageURL, want)
	}
}

func TestBMCDiskStatusListsSlotsOverrideAndRemoteMedia(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.slots[0].inserted = true
	bmc.slots[0].image = "//10.0.0.9/debian.iso"
	bmc.boot["BootSourceOverrideEnabled"] = "Continuous"
	bmc.boot["BootSourceOverrideTarget"] = "Cd"
	bmc.boot["BootSourceOverrideMode"] = "UEFI"
	driver, _, out := newMediaTestBMC(t, bmc)

	if err := driver.DiskStatus(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"CD1", "//10.0.0.9/debian.iso", "USBStick2", "insert,eject",
		"Boot override: Cd, Continuous, UEFI",
		"Remote media on Self: Enabled",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status = %q, want %q in it", out, want)
		}
	}
}

func TestBMCDiskServeServesTheImageUntilTheContextEnds(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.fetch = true
	driver, progress, _ := newMediaTestBMC(t, bmc)
	image := filepath.Join(t.TempDir(), "debian.iso")
	if err := os.WriteFile(image, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- driver.DiskServe(ctx, image, DiskServeOptions{Listen: "127.0.0.1:0"})
	}()
	waitFor(t, func() bool { return strings.Contains(progress.String(), DiskReady) }, DiskReady)
	bmc.mu.Lock()
	fetched := slices.Clone(bmc.fetched)
	imageURL, _ := bmc.inserts[0]["Image"].(string)
	bmc.mu.Unlock()
	if !strings.HasPrefix(imageURL, "https://127.0.0.1:") || !strings.HasSuffix(imageURL, "/debian.iso") {
		t.Errorf("image URL = %q, want https://127.0.0.1:<port>/debian.iso", imageURL)
	}
	if len(fetched) != 3 || !strings.Contains(fetched[2], `206 "2345"`) {
		t.Errorf("fetched = %q, want HEAD, HEAD and a ranged GET answered 206", fetched)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("%v\n%s", err, progress)
	}
	if cd := bmc.slot("CD1"); cd.inserted {
		t.Error("CD1 still inserted after the serve ended")
	}
	if got := bmc.boot["BootSourceOverrideEnabled"]; got != "Disabled" {
		t.Errorf("boot override = %v after the serve, want Disabled", got)
	}
}

func TestBMCDiskServeExplainsAnImageTheBMCNeverFetched(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.failInsert = true
	driver, _, _ := newMediaTestBMC(t, bmc)
	image := filepath.Join(t.TempDir(), "debian.iso")
	if err := os.WriteFile(image, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := driver.DiskServe(context.Background(), image, DiskServeOptions{Listen: "127.0.0.1:0"})
	if err == nil {
		t.Fatal("DiskServe: want an error, got nil")
	}
	for _, want := range []string{"The image server got no request for https://127.0.0.1:", "--listen :443", "REDIRECT", "--media-url https://127.0.0.1/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q in it", err, want)
		}
	}
}

func TestBMCDiskServeGivesNoFetchHintForARejectedInsert(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.rejectInsert = true
	driver, _, _ := newMediaTestBMC(t, bmc)
	image := filepath.Join(t.TempDir(), "debian.iso")
	if err := os.WriteFile(image, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := driver.DiskServe(context.Background(), image, DiskServeOptions{Listen: "127.0.0.1:0"})
	if err == nil || !strings.Contains(err.Error(), "ActionParameterValueFormatError") {
		t.Fatalf("DiskServe = %v, want the BMC's refusal", err)
	}
	if strings.Contains(err.Error(), "got no request") {
		t.Errorf("error = %q, want no fetch hint for an insert the BMC refused", err)
	}
}

func TestBMCDiskMountRefusesAProtocolTheActionInfoLeavesOut(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.protocols = []string{"NFS", "CIFS"}
	driver, _, _ := newMediaTestBMC(t, bmc)

	err := driver.DiskMount(context.Background(), "https://10.0.0.9/debian.iso", DiskOptions{})
	if err == nil || !strings.Contains(err.Error(), "CD1 takes images over NFS, CIFS, not HTTPS") {
		t.Fatalf("DiskMount = %v, want HTTPS refused by the ActionInfo", err)
	}
	if len(bmc.resets) != 0 || len(bmc.inserts) != 0 {
		t.Errorf("resets = %q, inserts = %v, want the host left alone and no insert", bmc.resets, bmc.inserts)
	}

	bmc.protocols = []string{"NFS", "HTTPS"}
	if err := driver.DiskMount(context.Background(), "https://10.0.0.9/debian.iso", DiskOptions{}); err != nil {
		t.Errorf("DiskMount with HTTPS listed = %v", err)
	}
}

func TestBMCDiskMountHintsAtNFSForAUSBInsertOverHTTPS(t *testing.T) {
	bmc := amiMediaBMC()
	bmc.failInsert = true
	driver, _, _ := newMediaTestBMC(t, bmc)

	err := driver.DiskMount(context.Background(), "https://10.0.0.9/disk.img", DiskOptions{Medium: DiskUSB})
	if err == nil || !strings.Contains(err.Error(), "Some BMCs take USB images only over NFS or CIFS") {
		t.Errorf("DiskMount on USB = %v, want the USB hint", err)
	}

	err = driver.DiskMount(context.Background(), "https://10.0.0.9/debian.iso", DiskOptions{Medium: DiskCD})
	if err == nil || strings.Contains(err.Error(), "USB images") {
		t.Errorf("DiskMount on CD = %v, want no USB hint", err)
	}
}

func TestBMCMediaURLNeedsSettingThroughAJumpHost(t *testing.T) {
	driver, _, _ := newMediaTestBMC(t, amiMediaBMC())
	driver.access.Jump = "jumphost"
	if _, err := driver.mediaBase("", "8443"); err == nil || !strings.Contains(err.Error(), "--media-url") {
		t.Errorf("mediaBase through a jump host = %v, want an error naming --media-url", err)
	}
	for mediaURL, want := range map[string]string{"https://10.0.0.9/media/": "/media/x.iso", "https://10.0.0.9": "/x.iso"} {
		base, err := driver.mediaBase(mediaURL, "8443")
		if err != nil || base.JoinPath("x.iso").Path != want {
			t.Errorf("mediaBase(%q) = %v, %v, want the image at %s", mediaURL, base, err, want)
		}
	}
	driver.access.Jump = ""
	for port, want := range map[string]string{"8443": "https://127.0.0.1:8443/", "443": "https://127.0.0.1/"} {
		base, err := driver.mediaBase("", port)
		if err != nil || base.String() != want {
			t.Errorf("mediaBase(port %s) = %v, %v, want %s", port, base, err, want)
		}
	}
}

func TestSameImageMatchesAMIsSchemelessImage(t *testing.T) {
	if !sameImage("//10.0.0.9/images/debian.iso", "https://10.0.0.9:8443/images/debian.iso") {
		t.Error("//host/path does not match the URL it came from")
	}
	if sameImage("//10.0.0.9/images/other.iso", "https://10.0.0.9/images/debian.iso") {
		t.Error("a different path matched")
	}
}

func TestImageServerServesOneFileWithRanges(t *testing.T) {
	file := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(file, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	progress := &syncWriter{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := startImageServer(file, "/disk.img", listener, "127.0.0.1", progress)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	base := fmt.Sprintf("https://127.0.0.1:%d", srv.port())

	if srv.requested() {
		t.Error("requested before any request")
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/disk.img", nil)
	req.Header.Set("Range", "bytes=3-6")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(data) != "3456" {
		t.Errorf("ranged GET = %d %q, want 206 \"3456\"", resp.StatusCode, data)
	}
	resp, err = client.Head(base + "/disk.img")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 10 {
		t.Errorf("HEAD = %d, length %d, want 200 and 10", resp.StatusCode, resp.ContentLength)
	}
	resp, err = client.Get(base + "/other.img")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET of another path = %d, want 404", resp.StatusCode)
	}
	if !srv.requested() {
		t.Error("requested() false after requests")
	}
	if n := strings.Count(progress.String(), "first request from 127.0.0.1"); n != 1 {
		t.Errorf("log = %q, want the first request from 127.0.0.1 logged once", progress)
	}
}
