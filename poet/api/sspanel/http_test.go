package sspanel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/poet/api"
)

func TestReportUserTrafficRequestShape(t *testing.T) {
	requests := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		if request.URL.Path != "/mod_mu/users/traffic" {
			t.Errorf("path = %s, want /mod_mu/users/traffic", request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("key") != "test-key" || query.Get("muKey") != "test-key" || query.Get("node_id") != "3" {
			t.Errorf("query = %v, want key, muKey and node_id", query)
		}
		var body struct {
			Data []UserTraffic `json:"data"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		} else if len(body.Data) != 2 || body.Data[0] != (UserTraffic{UID: 101, Upload: 123, Download: 456}) || body.Data[1] != (UserTraffic{UID: 102, Upload: 789, Download: 987}) {
			t.Errorf("body data = %#v", body.Data)
		}
		requests <- struct{}{}
		writePanelResponse(writer, http.StatusOK, 1)
	}))
	defer server.Close()

	client := newHTTPTestClient(server.URL)
	traffic := []api.UserTraffic{
		{UID: 101, Email: "one@example.com", Upload: 123, Download: 456},
		{UID: 102, Email: "two@example.com", Upload: 789, Download: 987},
	}
	if err := client.ReportUserTraffic(&traffic); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("traffic request was not received")
	}
}

func TestReportNodeOnlineUsersRequestShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/mod_mu/users/aliveip" {
			t.Errorf("path = %s, want /mod_mu/users/aliveip", request.URL.Path)
		}
		var body struct {
			Data []OnlineUser `json:"data"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		} else if len(body.Data) != 2 || body.Data[0] != (OnlineUser{UID: 101, IP: "192.0.2.1"}) || body.Data[1] != (OnlineUser{UID: 101, IP: "2001:db8::1"}) {
			t.Errorf("body data = %#v", body.Data)
		}
		writePanelResponse(writer, http.StatusOK, 1)
	}))
	defer server.Close()

	client := newHTTPTestClient(server.URL)
	online := []api.OnlineUser{{UID: 101, IP: "192.0.2.1"}, {UID: 101, IP: "2001:db8::1"}}
	if err := client.ReportNodeOnlineUsers(&online); err != nil {
		t.Fatal(err)
	}
	if client.LastReportOnline[101] != 2 {
		t.Fatalf("LastReportOnline = %v, want UID 101 count 2", client.LastReportOnline)
	}
}

func TestPanelClientRetriesConnectionError(t *testing.T) {
	client := newHTTPTestClient("http://panel.invalid")
	client.client.SetRetryWaitTime(time.Millisecond)
	var attempts atomic.Int32
	client.client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempt := attempts.Add(1)
		if attempt < 3 {
			return nil, errors.New("simulated connection reset")
		}
		return panelHTTPResponse(request, http.StatusOK, 1), nil
	}))

	traffic := []api.UserTraffic{{UID: 101, Upload: 1, Download: 2}}
	if err := client.ReportUserTraffic(&traffic); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

func TestPanelClientDoesNotRetryTimeout(t *testing.T) {
	client := newHTTPTestClient("http://panel.invalid")
	client.client.SetRetryWaitTime(time.Millisecond)
	var attempts atomic.Int32
	client.client.SetTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, testTimeoutError{}
	}))

	traffic := []api.UserTraffic{{UID: 101, Upload: 1, Download: 2}}
	if err := client.ReportUserTraffic(&traffic); err == nil {
		t.Fatal("expected timeout error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func TestPanelClientDoesNotRetryContextDeadline(t *testing.T) {
	client := newHTTPTestClient("http://panel.invalid")
	client.client.SetRetryWaitTime(time.Millisecond)
	var attempts atomic.Int32
	client.client.SetTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, context.DeadlineExceeded
	}))

	traffic := []api.UserTraffic{{UID: 101, Upload: 1, Download: 2}}
	if err := client.ReportUserTraffic(&traffic); err == nil {
		t.Fatal("expected deadline error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func TestPanelClientDoesNotRetryHTTPResponses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := newHTTPTestClient("http://panel.invalid")
			client.client.SetRetryWaitTime(time.Millisecond)
			var attempts atomic.Int32
			client.client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts.Add(1)
				return panelHTTPResponse(request, status, 0), nil
			}))

			traffic := []api.UserTraffic{{UID: 101, Upload: 1, Download: 2}}
			if err := client.ReportUserTraffic(&traffic); err == nil {
				t.Fatal("expected HTTP response error")
			}
			if attempts.Load() != 1 {
				t.Fatalf("attempts = %d, want 1", attempts.Load())
			}
		})
	}
}

func TestPanelClientDoesNotRetryBusinessError(t *testing.T) {
	client := newHTTPTestClient("http://panel.invalid")
	client.client.SetRetryWaitTime(time.Millisecond)
	var attempts atomic.Int32
	client.client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return panelHTTPResponse(request, http.StatusOK, 0), nil
	}))

	traffic := []api.UserTraffic{{UID: 101, Upload: 1, Download: 2}}
	if err := client.ReportUserTraffic(&traffic); err == nil {
		t.Fatal("expected panel business error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func TestPanelETagsRoundTripAnd304(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		etagKey   string
		data      string
		fetch     func(*APIClient) error
		notModify string
	}{
		{
			name: "node", path: "/mod_mu/nodes/3/info", etagKey: "node",
			data:      `{"server":"example.com;14555;0;ws;;path=/anytls","version":"2020.1"}`,
			fetch:     func(client *APIClient) error { _, err := client.GetNodeInfo(); return err },
			notModify: api.NodeNotModified,
		},
		{
			name: "users", path: "/mod_mu/users", etagKey: "users",
			data:      `[{"id":101,"email":"one@example.com","uuid":"uuid-101"}]`,
			fetch:     func(client *APIClient) error { _, err := client.GetUserList(); return err },
			notModify: api.UserNotModified,
		},
		{
			name: "rules", path: "/mod_mu/func/detect_rules", etagKey: "rules",
			data:      `[{"id":7,"regex":"example\\.com"}]`,
			fetch:     func(client *APIClient) error { _, err := client.GetNodeRule(); return err },
			notModify: api.RuleNotModified,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != test.path {
					t.Errorf("path = %s, want %s", request.URL.Path, test.path)
				}
				attempt := requests.Add(1)
				if attempt == 1 {
					if got := request.Header.Get("If-None-Match"); got != "" {
						t.Errorf("first If-None-Match = %q, want empty", got)
					}
					writeRawPanelData(writer, http.StatusOK, `"v1"`, test.data)
					return
				}
				if got := request.Header.Get("If-None-Match"); got != `"v1"` {
					t.Errorf("second If-None-Match = %q, want %q", got, `"v1"`)
				}
				writer.WriteHeader(http.StatusNotModified)
			}))
			defer server.Close()

			client := newHTTPTestClient(server.URL)
			if err := test.fetch(client); err != nil {
				t.Fatalf("first fetch: %v", err)
			}
			if client.eTags[test.etagKey] != `"v1"` {
				t.Fatalf("stored ETag = %q, want %q", client.eTags[test.etagKey], `"v1"`)
			}
			if err := test.fetch(client); err == nil || err.Error() != test.notModify {
				t.Fatalf("second fetch error = %v, want %q", err, test.notModify)
			}
		})
	}
}

func TestInvalidPanelPayloadDoesNotCommitETag(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		etagKey string
		data    string
		fetch   func(*APIClient) error
	}{
		{
			name: "node JSON", path: "/mod_mu/nodes/3/info", etagKey: "node", data: `"not-an-object"`,
			fetch: func(client *APIClient) error { _, err := client.GetNodeInfo(); return err },
		},
		{
			name: "users JSON", path: "/mod_mu/users", etagKey: "users", data: `{"not":"a-list"}`,
			fetch: func(client *APIClient) error { _, err := client.GetUserList(); return err },
		},
		{
			name: "invalid rule regex", path: "/mod_mu/func/detect_rules", etagKey: "rules", data: `[{"id":7,"regex":"["}]`,
			fetch: func(client *APIClient) error { _, err := client.GetNodeRule(); return err },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != test.path {
					t.Errorf("path = %s, want %s", request.URL.Path, test.path)
				}
				writeRawPanelData(writer, http.StatusOK, `"invalid-version"`, test.data)
			}))
			defer server.Close()
			client := newHTTPTestClient(server.URL)
			if err := test.fetch(client); err == nil {
				t.Fatal("expected invalid payload error")
			}
			if got := client.eTags[test.etagKey]; got != "" {
				t.Fatalf("invalid payload committed ETag %q", got)
			}
		})
	}
}

func newHTTPTestClient(host string) *APIClient {
	client := New(&api.Config{APIHost: host, Key: "test-key", NodeID: 3, NodeType: "V2ray"})
	client.client.SetRetryWaitTime(time.Millisecond)
	client.client.SetRetryMaxWaitTime(time.Millisecond)
	return client
}

func writePanelResponse(writer http.ResponseWriter, status int, ret uint) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(Response{Ret: ret})
}

func writeRawPanelData(writer http.ResponseWriter, status int, etag, data string) {
	writer.Header().Set("Content-Type", "application/json")
	if etag != "" {
		writer.Header().Set("ETag", etag)
	}
	writer.WriteHeader(status)
	_, _ = fmt.Fprintf(writer, `{"ret":1,"data":%s}`, data)
}

func panelHTTPResponse(request *http.Request, status int, ret uint) *http.Response {
	body, _ := json.Marshal(Response{Ret: ret})
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    request,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type testTimeoutError struct{}

func (testTimeoutError) Error() string   { return "simulated timeout" }
func (testTimeoutError) Timeout() bool   { return true }
func (testTimeoutError) Temporary() bool { return true }
