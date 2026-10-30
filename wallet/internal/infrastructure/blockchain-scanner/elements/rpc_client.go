package elements_scanner

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"
)

// rpcMaxConns bounds the connections the client holds to the node, in use and
// idle together. Every account's scanner shares one client: unbounded, a busy
// node (its work queue full, its answers slow) had the client open a connection
// per waiting call, thousands at once, until the process ran out of file
// descriptors ("socket: too many open files"). A call beyond the bound waits for
// a free connection, within its timeout. The node serves a few calls at a time
// anyway (-rpcthreads).
const rpcMaxConns = 16

// newHTTPClient is the node client's HTTP client: its timeout covers a whole
// call (the wait for a connection, the request and the body read), so a call
// that times out is cancelled and its connection closed or given back, never
// left with a response nobody reads.
func newHTTPClient(useSSL bool, timeout int) *http.Client {
	t := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        rpcMaxConns,
		MaxIdleConnsPerHost: rpcMaxConns,
		MaxConnsPerHost:     rpcMaxConns,
		IdleConnTimeout:     60 * time.Second,
	}
	if useSSL {
		// #nosec G402
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{Transport: t, Timeout: time.Duration(timeout) * time.Second}
}

// rpcTimeoutSeconds is the default per-request HTTP timeout for the node JSON-RPC
// client. It must be generous enough for large `getblock`/`getblockfilter`
// responses and for the initial getblockchaininfo handshake when the node is
// still busy at startup; the historical 5s value was too low and made
// CreateWallet/GetLatestBlock fail with a request timeout.
const rpcTimeoutSeconds = 30

// RpcClient represents a JSON RPC client (over HTTP(s)).
type rpcClient struct {
	serverAddr string
	httpClient *http.Client
	timeout    int
}

// rpcRequest represent a RCP request
type rpcRequest struct {
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	Id      int64       `json:"id"`
	JsonRpc string      `json:"jsonrpc"`
}

type rpcResponse struct {
	Id     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Err    interface{}     `json:"error"`
}

func newRpcClient(
	addr string, timeout int,
) (c *rpcClient, err error) {
	c = &rpcClient{
		serverAddr: addr,
		httpClient: newHTTPClient(strings.HasPrefix(addr, "https"), timeout),
		timeout:    timeout,
	}

	if _, err := c.call("getblockchaininfo", nil); err != nil {
		return nil, fmt.Errorf("failed to connect to node at %s", addr)
	}

	return
}

func (c *rpcClient) call(
	method string, params []interface{},
) (interface{}, error) {
	_, resp, err := c.handleRPCRequest(method, params)
	if err != nil {
		return nil, err
	}
	// A 200 response can still carry a JSON-RPC error (result null, error set);
	// surface it so callers never type-assert a nil result into a string/map.
	if resp.Err != nil {
		return nil, fmt.Errorf("method %s returned error: %v", method, resp.Err)
	}
	var out interface{}
	err = json.Unmarshal(resp.Result, &out)
	if err != nil {
		return nil, err
	}

	return out, nil
}

// Call prepare & exec the request
func (c *rpcClient) handleRPCRequest(
	method string, params interface{},
) (status int, rr rpcResponse, err error) {
	status = http.StatusInternalServerError
	rpcR := rpcRequest{method, params, time.Now().UnixNano(), "1.0"}
	payloadBuffer := &bytes.Buffer{}
	if err = json.NewEncoder(payloadBuffer).Encode(rpcR); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.timeout)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", c.serverAddr, payloadBuffer)
	if err != nil {
		return
	}
	req.Header.Add("Content-Type", "application/json;charset=utf-8")
	req.Header.Add("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("method %s: request timed out after %ds", method, c.timeout)
		}
		return
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	if resp.StatusCode != http.StatusOK {
		// The node does not always return a JSON {"error":{"message":...}} body on
		// a non-200 response: under load Elements/Sequentia replies with a plain
		// "Work queue depth exceeded" (HTTP 500), and proxies/timeouts can yield
		// other non-JSON bodies. Extract the message defensively and fall back to
		// the raw body, never asserting on a possibly-nil field. The previous
		// out["error"]["message"].(string) panicked on those bodies and crashed the
		// whole wallet, which systemd then restarted into a fresh full re-scan,
		// re-overloading the node in a crash loop.
		msg := strings.TrimSpace(string(data))
		out := map[string]map[string]interface{}{}
		if json.Unmarshal(data, &out) == nil {
			if e, ok := out["error"]; ok {
				if m, ok := e["message"].(string); ok && m != "" {
					msg = m
				}
			}
		}
		if msg == "" {
			msg = resp.Status
		}
		err = fmt.Errorf(
			"method %s failed with status %d: %s", method, resp.StatusCode, msg,
		)
		status = resp.StatusCode
		return
	}

	if err = json.Unmarshal(data, &rr); err != nil {
		return
	}

	status = resp.StatusCode
	return
}
