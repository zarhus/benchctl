// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return &Client{BaseURL: server.URL, User: "root", Password: "pw", HTTP: server.Client()}
}

func TestSystemPathNamesEverySystemWhenThereAreSeveral(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Members":[{"@odata.id":"/redfish/v1/Systems/System_0"},{"@odata.id":"/redfish/v1/Systems/HGX_Baseboard_0"}]}`)
	})
	_, err := client.SystemPath("")
	if err == nil || !strings.Contains(err.Error(), "System_0, HGX_Baseboard_0") {
		t.Errorf("SystemPath = %v, want both IDs named", err)
	}
}

func TestSystemPathUsesTheSettingWithoutARequest(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	})
	path, err := client.SystemPath("System_0")
	if err != nil || path != "/redfish/v1/Systems/System_0" {
		t.Errorf("SystemPath = %q, %v", path, err)
	}
}

func TestResetPostsTheResetType(t *testing.T) {
	var body, contentType string
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body, contentType = string(data), r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})
	if err := client.Reset("/redfish/v1/Systems/system/Actions/ComputerSystem.Reset", "ForceOff"); err != nil {
		t.Fatal(err)
	}
	if body != `{"ResetType":"ForceOff"}` || contentType != "application/json" {
		t.Errorf("body %q, content type %q", body, contentType)
	}
}

func TestErrorsCarryTheBMCsMessage(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"Base.1.8.GeneralError","message":"A general error has occurred.","@Message.ExtendedInfo":[{"Message":"The value ForceRestart for the parameter ResetType is not in the list of acceptable values."}]}}`)
	})
	err := client.Reset("/x", "ForceRestart")
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "not in the list of acceptable values") {
		t.Errorf("Reset = %v, want the status and the extended message", err)
	}
}

func TestBadCredentialsReportTheStatus(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	var out System
	if err := client.Get("/redfish/v1/Systems/system", &out); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("Get = %v, want 401", err)
	}
}

func TestNewHTTPClientRejectsAnEmptyCABundle(t *testing.T) {
	path := t.TempDir() + "/ca.pem"
	if err := writeFile(path, "not a certificate"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHTTPClient(Transport{CAFile: path}); err == nil {
		t.Error("NewHTTPClient accepted a CA bundle with no certificates")
	}
}

func TestCmdConnCarriesBytesBothWays(t *testing.T) {
	conn, err := startConn(osexec.Command("cat"), "jump", "bmc:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Errorf("read %q, %v, want ping", buf, err)
	}
}

func TestCmdConnReportsWhySSHEnded(t *testing.T) {
	cmd := osexec.Command("sh", "-c", "echo 'channel 0: open failed: connect failed: No route to host' >&2")
	conn, err := startConn(cmd, "jump", "bmc:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "No route to host") || !strings.Contains(err.Error(), "ssh -W bmc:443 jump") {
		t.Errorf("Read = %v, want ssh's reason", err)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
