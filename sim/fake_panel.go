package sim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/sagernet/sing-box/poet/api/sspanel"
)

// FakePanel impersonates an old SSPanel /mod_mu API surface, recording
// everything the SingR controller sends to /users/traffic so the sim can
// compare reported numbers to ground truth.
type FakePanel struct {
	*httptest.Server

	NodeID     int
	ListenPort int
	UserUID    int
	UserUUID   string
	UserPasswd string

	mu              sync.Mutex
	trafficRequests []TrafficReportBody // every body received at /mod_mu/users/traffic
	aliveipCount    int
	statusCount     int
}

// TrafficReportBody mirrors the JSON shape SingR's APIClient.ReportUserTraffic posts.
type TrafficReportBody struct {
	Data []sspanel.UserTraffic `json:"data"`
}

// NewFakePanel starts an httptest.Server. nodeID is the panel node id used in
// /mod_mu/nodes/{nodeID}/info paths. listenPort is what the fake panel will
// advertise inside the V2ray server string so SingR points its AnyTLS at it.
func NewFakePanel(nodeID, listenPort, userUID int, userUUID, userPasswd string) *FakePanel {
	fp := &FakePanel{
		NodeID:     nodeID,
		ListenPort: listenPort,
		UserUID:    userUID,
		UserUUID:   userUUID,
		UserPasswd: userPasswd,
	}
	mux := http.NewServeMux()

	// GET /mod_mu/nodes/{id}/info -> V2ray-style raw server string with anytls alias.
	mux.HandleFunc(fmt.Sprintf("/mod_mu/nodes/%d/info", nodeID), func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			rawServer := fmt.Sprintf("127.0.0.1;%d;0;ws;;path=/anytls", listenPort)
			data, _ := json.Marshal(&sspanel.NodeInfoResponse{
				SpeedLimit:      0,
				TrafficRate:     1,
				RawServerString: rawServer,
				Type:            "v2ray",
				Version:         "2024.1", // > 2021.11 so DisableCustomConfig path used; we set DisableCustomConfig=true in client to take ParseV2rayNodeResponse path
			})
			writeRet1(w, data)
		case http.MethodPost:
			fp.mu.Lock()
			fp.statusCount++
			fp.mu.Unlock()
			writeRet1(w, json.RawMessage(`null`))
		default:
			http.NotFound(w, r)
		}
	})

	// GET /mod_mu/users -> single user.
	mux.HandleFunc("/mod_mu/users", func(w http.ResponseWriter, r *http.Request) {
		users := []sspanel.UserResponse{
			{
				ID:            userUID,
				Email:         "sim@example.com",
				Passwd:        userPasswd,
				UUID:          userUUID,
				Port:          uint32(listenPort),
				Method:        "aes-128-gcm",
				NodeConnector: 5,
			},
		}
		data, _ := json.Marshal(users)
		writeRet1(w, data)
	})

	// POST /mod_mu/users/traffic -> record body.
	mux.HandleFunc("/mod_mu/users/traffic", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed TrafficReportBody
		if err := json.Unmarshal(body, &parsed); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fp.mu.Lock()
		fp.trafficRequests = append(fp.trafficRequests, parsed)
		fp.mu.Unlock()
		writeRet1(w, json.RawMessage(`null`))
	})

	// POST /mod_mu/users/aliveip
	mux.HandleFunc("/mod_mu/users/aliveip", func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		fp.aliveipCount++
		fp.mu.Unlock()
		writeRet1(w, json.RawMessage(`null`))
	})

	fp.Server = httptest.NewServer(mux)
	return fp
}

func writeRet1(w http.ResponseWriter, data json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	resp := sspanel.Response{Ret: 1, Data: data}
	_ = json.NewEncoder(w).Encode(&resp)
}

// TrafficReports returns all bodies received at /users/traffic so far.
func (fp *FakePanel) TrafficReports() []TrafficReportBody {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	out := make([]TrafficReportBody, len(fp.trafficRequests))
	copy(out, fp.trafficRequests)
	return out
}

// SumReported sums Upload/Download across all received traffic reports for the
// given UID. This is the ground truth of "what the panel saw on the wire".
func (fp *FakePanel) SumReported(uid int) (upload, download int64) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	for _, body := range fp.trafficRequests {
		for _, rec := range body.Data {
			if rec.UID == uid {
				upload += rec.Upload
				download += rec.Download
			}
		}
	}
	return
}
