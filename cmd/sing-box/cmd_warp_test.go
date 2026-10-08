package main

import (
	"context"
	stdjson "encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
)

const warpTestRegistration = `{
  "id": "dev-id",
  "token": "dev-token",
  "config": {
    "client_id": "AQID",
    "peers": [{
      "public_key": "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
      "endpoint": {"v4": "162.159.192.7:0", "v6": "[2606:4700:d0::a29f:c007]:0", "host": "engage.cloudflareclient.com:2408"}
    }],
    "interface": {"addresses": {"v4": "172.16.0.2", "v6": "2606:4700:110:8a36::1"}}
  }
}`

func TestWarpParseRegistration(t *testing.T) {
	for name, content := range map[string]string{
		"bare":    warpTestRegistration,
		"wrapped": `{"success": true, "result": ` + warpTestRegistration + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			account, err := warpParseRegistration([]byte(content), "priv")
			if err != nil {
				t.Fatal(err)
			}
			if account.ID != "dev-id" || account.Token != "dev-token" || account.PrivateKey != "priv" {
				t.Fatalf("identity: %+v", account)
			}
			if got := strings.Join(account.Address, ","); got != "172.16.0.2/32,2606:4700:110:8a36::1/128" {
				t.Fatalf("address: %s", got)
			}
			if account.EndpointV4 != "162.159.192.7" || account.EndpointV6 != "2606:4700:d0::a29f:c007" {
				t.Fatalf("endpoint: %s %s", account.EndpointV4, account.EndpointV6)
			}
			if len(account.Reserved) != 3 || account.Reserved[0] != 1 || account.Reserved[1] != 2 || account.Reserved[2] != 3 {
				t.Fatalf("reserved: %v", account.Reserved)
			}
		})
	}
}

func TestWarpParseRegistrationRejectsIncomplete(t *testing.T) {
	for name, content := range map[string]string{
		"no peer":      `{"config": {"interface": {"addresses": {"v4": "172.16.0.2"}}}}`,
		"no address":   `{"config": {"peers": [{"public_key": "k"}]}}`,
		"bad clientid": `{"config": {"client_id": "AQIDBA==", "peers": [{"public_key": "k"}], "interface": {"addresses": {"v4": "172.16.0.2"}}}}`,
	} {
		if _, err := warpParseRegistration([]byte(content), "priv"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestWarpRegisterRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0a1922/reg" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("CF-Client-Version") == "" {
			t.Error("missing CF-Client-Version")
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]string
		if err := stdjson.Unmarshal(body, &request); err != nil || request["key"] == "" || request["tos"] == "" {
			t.Errorf("body: %s", body)
		}
		io.WriteString(w, warpTestRegistration)
	}))
	defer server.Close()
	account, err := warpRegister(server.URL + "/v0a1922/")
	if err != nil {
		t.Fatal(err)
	}
	if len(account.PrivateKey) != 44 {
		t.Fatalf("private key: %q", account.PrivateKey)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "too many", http.StatusTooManyRequests)
	}))
	defer failing.Close()
	if _, err := warpRegister(failing.URL); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected HTTP 429 error, got %v", err)
	}
}

// The rendered endpoint must decode through sing-box's own option parser —
// that is what `run` will do with it once the shell writes it to server.json.
func TestWarpEndpointDecodes(t *testing.T) {
	account, err := warpParseRegistration([]byte(warpTestRegistration), "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=")
	if err != nil {
		t.Fatal(err)
	}
	for _, via := range []string{"v4", "v6"} {
		endpoint, err := account.endpoint("warp", via)
		if err != nil {
			t.Fatal(err)
		}
		content, _ := stdjson.Marshal(map[string]any{"endpoints": []any{endpoint}})
		options, err := json.UnmarshalExtendedContext[option.Options](include.Context(context.Background()), content)
		if err != nil {
			t.Fatalf("%s: %v", via, err)
		}
		wg, ok := options.Endpoints[0].Options.(*option.WireGuardEndpointOptions)
		if !ok {
			t.Fatalf("%s: options type %T", via, options.Endpoints[0].Options)
		}
		peer := wg.Peers[0]
		wantAddress := map[string]string{"v4": "162.159.192.7", "v6": "2606:4700:d0::a29f:c007"}[via]
		if peer.Address != wantAddress || peer.Port != 2408 || len(wg.Address) != 2 {
			t.Fatalf("%s: peer %+v address %v", via, peer, wg.Address)
		}
		if len(peer.Reserved) != 3 || peer.Reserved[2] != 3 {
			t.Fatalf("%s: reserved %v", via, peer.Reserved)
		}
	}
	if _, err := account.endpoint("warp", "auto"); err == nil {
		t.Fatal("endpoint must reject via=auto")
	}
}

func TestWarpParseTrace(t *testing.T) {
	result := warpParseTrace("fl=1\nip=104.28.1.2\nloc=SG\ncolo=SIN\nwarp=on\n")
	if !result.OK || result.IP != "104.28.1.2" || result.Colo != "SIN" || result.Loc != "SG" {
		t.Fatalf("on: %+v", result)
	}
	if result := warpParseTrace("ip=1.2.3.4\nwarp=off\n"); result.OK || result.Error == "" {
		t.Fatalf("off: %+v", result)
	}
}
