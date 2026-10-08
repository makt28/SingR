package main

// SingR: Cloudflare WARP helpers behind `singr warp ...` in SingR.sh /
// SingR-docker.sh. The shell owns server.json; this file only does what the
// shell cannot do without new host dependencies:
//
//   - register: X25519 key pair + the Cloudflare client registration call
//   - endpoint: render the wireguard endpoint JSON for a chosen peer family
//   - test:     bring that exact endpoint up in-process and fetch
//               cdn-cgi/trace through it, over IPv4 and IPv6 targets
//
// `test` exists because a WireGuard handshake failure does not fail startup:
// the process stays active while everything routed to WARP black-holes, so the
// systemd/docker liveness checks cannot catch it. It runs before the shell
// writes any config.
//
// All three speak JSON on stdin/stdout so the docker backend can run them as a
// throwaway `docker run -i` without mounting anything.

import (
	"bytes"
	"context"
	"encoding/base64"
	stdjson "encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"

	"github.com/spf13/cobra"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const (
	warpPeerPublicKey = "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo="
	warpEndpointV4    = "162.159.192.1"
	warpEndpointV6    = "2606:4700:d0::a29f:c001"
	warpPort          = 2408
	warpMTU           = 1280
	warpKeepalive     = 25

	// Same client identity wgcf uses for the (unofficial) client API.
	warpUserAgent     = "okhttp/3.12.1"
	warpClientVersion = "a-6.3-1922"

	warpTraceV4 = "https://1.1.1.1/cdn-cgi/trace"
	warpTraceV6 = "https://[2606:4700:4700::1111]/cdn-cgi/trace"
)

var (
	commandWarpFlagAPI     string
	commandWarpFlagVia     string
	commandWarpFlagTag     string
	commandWarpFlagTimeout time.Duration
)

var commandWarp = &cobra.Command{
	Use:   "warp",
	Short: "Cloudflare WARP helpers (SingR)",
}

var commandWarpRegister = &cobra.Command{
	Use:   "register",
	Short: "Register a WARP account and print it as JSON",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		account, err := warpRegister(commandWarpFlagAPI)
		if err != nil {
			log.Fatal(err)
		}
		err = warpWriteJSON(os.Stdout, account)
		if err != nil {
			log.Fatal(err)
		}
	},
}

var commandWarpEndpoint = &cobra.Command{
	Use:   "endpoint",
	Short: "Print the wireguard endpoint for the account on stdin",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		account, err := warpReadAccount(os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		endpoint, err := account.endpoint(commandWarpFlagTag, commandWarpFlagVia)
		if err != nil {
			log.Fatal(err)
		}
		err = warpWriteJSON(os.Stdout, endpoint)
		if err != nil {
			log.Fatal(err)
		}
	},
}

var commandWarpTest = &cobra.Command{
	Use:   "test",
	Short: "Handshake with WARP and fetch cdn-cgi/trace through it",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		account, err := warpReadAccount(os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		result, err := warpTest(account, commandWarpFlagVia, commandWarpFlagTimeout)
		if err != nil {
			log.Fatal(err)
		}
		err = warpWriteJSON(os.Stdout, result)
		if err != nil {
			log.Fatal(err)
		}
		if !result.OK {
			os.Exit(1)
		}
	},
}

func init() {
	commandWarpRegister.Flags().StringVar(&commandWarpFlagAPI, "api", "https://api.cloudflareclient.com/v0a1922", "client API base URL")
	commandWarpEndpoint.Flags().StringVar(&commandWarpFlagVia, "via", "v4", "peer address family: v4 or v6")
	commandWarpEndpoint.Flags().StringVar(&commandWarpFlagTag, "tag", "warp", "endpoint tag")
	commandWarpTest.Flags().StringVar(&commandWarpFlagVia, "via", "auto", "peer address family: auto, v4 or v6")
	commandWarpTest.Flags().DurationVar(&commandWarpFlagTimeout, "timeout", 10*time.Second, "timeout per peer family")
	commandWarp.AddCommand(commandWarpRegister, commandWarpEndpoint, commandWarpTest)
	mainCommand.AddCommand(commandWarp)
}

// warpAccount is what the shell stores in warp.json (0600). ID and Token are
// only kept so the device can be managed/deleted later; the tunnel itself
// needs the rest.
type warpAccount struct {
	ID            string   `json:"id"`
	Token         string   `json:"token"`
	PrivateKey    string   `json:"private_key"`
	PeerPublicKey string   `json:"peer_public_key"`
	Reserved      []int    `json:"reserved,omitempty"`
	Address       []string `json:"address"`
	EndpointV4    string   `json:"endpoint_v4"`
	EndpointV6    string   `json:"endpoint_v6"`
	Port          uint16   `json:"port"`
}

// Rendered with plain encoding/json on purpose: option.WireGuardPeer's
// Reserved is []uint8, which encoding/json would emit as base64.
type warpEndpointJSON struct {
	Type       string         `json:"type"`
	Tag        string         `json:"tag"`
	MTU        uint32         `json:"mtu"`
	Address    []string       `json:"address"`
	PrivateKey string         `json:"private_key"`
	Peers      []warpPeerJSON `json:"peers"`
}

type warpPeerJSON struct {
	Address                     string   `json:"address"`
	Port                        uint16   `json:"port"`
	PublicKey                   string   `json:"public_key"`
	AllowedIPs                  []string `json:"allowed_ips"`
	PersistentKeepaliveInterval uint16   `json:"persistent_keepalive_interval"`
	Reserved                    []int    `json:"reserved,omitempty"`
}

func (a *warpAccount) endpoint(tag string, via string) (*warpEndpointJSON, error) {
	var peerAddress string
	switch via {
	case "v4":
		peerAddress = a.EndpointV4
	case "v6":
		peerAddress = a.EndpointV6
	default:
		return nil, E.New("invalid via: ", via, " (want v4 or v6)")
	}
	return &warpEndpointJSON{
		Type:       "wireguard",
		Tag:        tag,
		MTU:        warpMTU,
		Address:    a.Address,
		PrivateKey: a.PrivateKey,
		Peers: []warpPeerJSON{{
			Address:                     peerAddress,
			Port:                        a.Port,
			PublicKey:                   a.PeerPublicKey,
			AllowedIPs:                  []string{"0.0.0.0/0", "::/0"},
			PersistentKeepaliveInterval: warpKeepalive,
			Reserved:                    a.Reserved,
		}},
	}, nil
}

func warpReadAccount(reader io.Reader) (*warpAccount, error) {
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, E.Cause(err, "read account")
	}
	var account warpAccount
	err = stdjson.Unmarshal(content, &account)
	if err != nil {
		return nil, E.Cause(err, "decode account")
	}
	if account.PrivateKey == "" || len(account.Address) == 0 {
		return nil, E.New("incomplete account: missing private_key or address")
	}
	if account.PeerPublicKey == "" {
		account.PeerPublicKey = warpPeerPublicKey
	}
	if account.EndpointV4 == "" {
		account.EndpointV4 = warpEndpointV4
	}
	if account.EndpointV6 == "" {
		account.EndpointV6 = warpEndpointV6
	}
	if account.Port == 0 {
		account.Port = warpPort
	}
	return &account, nil
}

func warpWriteJSON(writer io.Writer, value any) error {
	encoder := stdjson.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

type warpRegResponse struct {
	ID     string `json:"id"`
	Token  string `json:"token"`
	Config struct {
		ClientID string `json:"client_id"`
		Peers    []struct {
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"endpoint"`
		} `json:"peers"`
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
	} `json:"config"`
}

func warpRegister(apiBase string) (*warpAccount, error) {
	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	requestBody, err := stdjson.Marshal(map[string]string{
		"key":        privateKey.PublicKey().String(),
		"install_id": "",
		"fcm_token":  "",
		"tos":        time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"model":      "PC",
		"type":       "Android",
		"locale":     "en_US",
	})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(apiBase, "/")+"/reg", bytes.NewReader(requestBody))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	request.Header.Set("User-Agent", warpUserAgent)
	request.Header.Set("CF-Client-Version", warpClientVersion)
	httpClient := &http.Client{Timeout: 30 * time.Second}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, E.Cause(err, "register")
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, E.Cause(err, "read register response")
	}
	if response.StatusCode/100 != 2 {
		return nil, E.New("register: HTTP ", response.StatusCode, ": ", strings.TrimSpace(string(content)))
	}
	return warpParseRegistration(content, privateKey.String())
}

func warpParseRegistration(content []byte, privateKey string) (*warpAccount, error) {
	// Newer API versions wrap the device in {"result": {...}, "success": true}.
	var wrapper struct {
		Result stdjson.RawMessage `json:"result"`
	}
	if stdjson.Unmarshal(content, &wrapper) == nil && len(wrapper.Result) > 0 && wrapper.Result[0] == '{' {
		content = wrapper.Result
	}
	var registration warpRegResponse
	err := stdjson.Unmarshal(content, &registration)
	if err != nil {
		return nil, E.Cause(err, "decode register response")
	}
	config := registration.Config
	if len(config.Peers) == 0 || config.Peers[0].PublicKey == "" {
		return nil, E.New("register response has no peer")
	}
	account := &warpAccount{
		ID:            registration.ID,
		Token:         registration.Token,
		PrivateKey:    privateKey,
		PeerPublicKey: config.Peers[0].PublicKey,
		EndpointV4:    warpEndpointHost(config.Peers[0].Endpoint.V4, warpEndpointV4),
		EndpointV6:    warpEndpointHost(config.Peers[0].Endpoint.V6, warpEndpointV6),
		Port:          warpPort,
	}
	if address, err := netip.ParseAddr(config.Interface.Addresses.V4); err == nil && address.Is4() {
		account.Address = append(account.Address, netip.PrefixFrom(address, 32).String())
	}
	if address, err := netip.ParseAddr(config.Interface.Addresses.V6); err == nil && address.Is6() {
		account.Address = append(account.Address, netip.PrefixFrom(address, 128).String())
	}
	if len(account.Address) == 0 {
		return nil, E.New("register response has no interface address")
	}
	if config.ClientID != "" {
		reserved, err := base64.StdEncoding.DecodeString(config.ClientID)
		if err != nil || len(reserved) != 3 {
			return nil, E.New("unexpected client_id: ", config.ClientID)
		}
		account.Reserved = []int{int(reserved[0]), int(reserved[1]), int(reserved[2])}
	}
	return account, nil
}

// The API reports peer endpoints as "162.159.192.1:0" / "[2606:...]:0"; only
// the address is useful, the port is always 2408.
func warpEndpointHost(value string, fallback string) string {
	if addrPort, err := netip.ParseAddrPort(value); err == nil {
		return addrPort.Addr().String()
	}
	if address, err := netip.ParseAddr(value); err == nil {
		return address.String()
	}
	return fallback
}

type warpTestResult struct {
	OK      bool            `json:"ok"`
	Via     string          `json:"via"`
	Results []warpViaResult `json:"results"`
}

type warpViaResult struct {
	Via  string          `json:"via"`
	OK   bool            `json:"ok"`
	IPv4 warpTraceResult `json:"ipv4"`
	IPv6 warpTraceResult `json:"ipv6"`
}

type warpTraceResult struct {
	OK    bool   `json:"ok"`
	IP    string `json:"ip,omitempty"`
	Warp  string `json:"warp,omitempty"`
	Colo  string `json:"colo,omitempty"`
	Loc   string `json:"loc,omitempty"`
	Error string `json:"error,omitempty"`
}

// warpTest tries each requested peer family in turn (auto = v4 then v6) and
// stops at the first one through which at least one trace came back via WARP.
func warpTest(account *warpAccount, via string, timeout time.Duration) (*warpTestResult, error) {
	var candidates []string
	switch via {
	case "auto":
		candidates = []string{"v4", "v6"}
	case "v4", "v6":
		candidates = []string{via}
	default:
		return nil, E.New("invalid via: ", via, " (want auto, v4 or v6)")
	}
	result := &warpTestResult{}
	for _, candidate := range candidates {
		viaResult, err := warpTestVia(account, candidate, timeout)
		if err != nil {
			return nil, err
		}
		result.Results = append(result.Results, *viaResult)
		if viaResult.OK {
			result.OK = true
			result.Via = candidate
			break
		}
	}
	return result, nil
}

func warpTestVia(account *warpAccount, via string, timeout time.Duration) (*warpViaResult, error) {
	endpointJSON, err := account.endpoint("warp", via)
	if err != nil {
		return nil, err
	}
	optionsJSON, err := stdjson.Marshal(map[string]any{
		"log":       map[string]any{"disabled": true},
		"endpoints": []any{endpointJSON},
	})
	if err != nil {
		return nil, err
	}
	// Parse through the same decoder `run` uses, so this is exactly the
	// endpoint server.json will get.
	options, err := json.UnmarshalExtendedContext[option.Options](globalCtx, optionsJSON)
	if err != nil {
		return nil, E.Cause(err, "decode endpoint")
	}
	ctx, cancel := context.WithCancel(service.ExtendContext(globalCtx))
	defer cancel()
	instance, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		return nil, E.Cause(err, "create endpoint")
	}
	defer instance.Close()
	// Box.Start would start the panel (and exit without -p). PreStart brings
	// the endpoint to StartStateStart through the outbound manager; PostStart
	// is what marks it ready to dial.
	err = instance.PreStart()
	if err != nil {
		return nil, E.Cause(err, "start endpoint")
	}
	err = instance.Endpoint().Start(adapter.StartStatePostStart)
	if err != nil {
		return nil, E.Cause(err, "start endpoint")
	}
	endpoint, loaded := instance.Endpoint().Get("warp")
	if !loaded {
		return nil, E.New("endpoint not found")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return endpoint.DialContext(ctx, network, M.ParseSocksaddr(addr))
		},
	}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: timeout}

	viaResult := &warpViaResult{Via: via}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		viaResult.IPv4 = warpTrace(httpClient, warpTraceV4)
	}()
	go func() {
		defer wg.Done()
		viaResult.IPv6 = warpTrace(httpClient, warpTraceV6)
	}()
	wg.Wait()
	viaResult.OK = viaResult.IPv4.OK || viaResult.IPv6.OK
	return viaResult, nil
}

func warpTrace(httpClient *http.Client, url string) warpTraceResult {
	response, err := httpClient.Get(url)
	if err != nil {
		return warpTraceResult{Error: err.Error()}
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return warpTraceResult{Error: err.Error()}
	}
	if response.StatusCode != http.StatusOK {
		return warpTraceResult{Error: "HTTP " + response.Status}
	}
	return warpParseTrace(string(content))
}

func warpParseTrace(content string) warpTraceResult {
	var result warpTraceResult
	for line := range strings.SplitSeq(content, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "ip":
			result.IP = value
		case "warp":
			result.Warp = value
		case "colo":
			result.Colo = value
		case "loc":
			result.Loc = value
		}
	}
	// A trace that answers with warp=off did not come through WARP at all.
	result.OK = result.Warp == "on" || result.Warp == "plus"
	if !result.OK {
		result.Error = "trace reports warp=" + result.Warp
	}
	return result
}
