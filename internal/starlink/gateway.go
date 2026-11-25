package starlink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/awlx/starlink-manager/api/device"
	"google.golang.org/protobuf/proto"
)

// Gateway errors surfaced to the web layer.
var (
	// ErrDeviceOffline maps DEVICE_CONNECTION_NOT_FOUND / DEVICE_NOT_CONNECTED:
	// the dish or router is not currently connected to the cloud.
	ErrDeviceOffline = errors.New("device offline: not connected to the Starlink cloud")
	// ErrGateway is a generic device gateway failure.
	ErrGateway = errors.New("device gateway call failed")
)

// gatewayURL is the gRPC-Web endpoint of the device gateway. It lives on the
// starlink.com web host; posting to api.* breaks the binary framing.
const gatewayPath = "/api/SpaceX.API.Device.Device/Handle"

// Gateway calls SpaceX.API.Device.Device/Handle through the Starlink account
// API's gRPC-Web device gateway. Requests carry a target_id so that dishes
// and routers which are unreachable on the LAN can still be controlled via
// the cloud ("ut"+userTerminalId for dishes, "Router-"+routerId for routers).
type Gateway struct {
	session *Session
	webBase string
	client  *http.Client
	log     *slog.Logger

	mu        sync.Mutex
	mutations map[string]*sync.Mutex // serialized mutations per target
}

// NewGateway creates a gateway against the given web base URL.
func NewGateway(session *Session, webBase string, client *http.Client, log *slog.Logger) *Gateway {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{
		session:   session,
		webBase:   strings.TrimRight(webBase, "/"),
		client:    client,
		log:       log,
		mutations: map[string]*sync.Mutex{},
	}
}

// mutationLock returns the per-target lock used to serialize mutations
// (reboots, stow, config changes) so overlapping commands cannot interleave.
func (g *Gateway) mutationLock(target string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := g.mutations[target]
	if !ok {
		m = &sync.Mutex{}
		g.mutations[target] = m
	}
	return m
}

// Handle sends one Request to the device gateway and returns the Response.
// On grpc-status 16 (unauthenticated) it refreshes the session and retries
// up to twice.
func (g *Gateway) Handle(ctx context.Context, req *device.Request) (*device.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := g.handleOnce(ctx, req)
		if err == nil {
			return resp, nil
		}
		var st *grpcStatusError
		if errors.As(err, &st) && st.code == 16 && attempt < 2 {
			// Token expired mid-call: force a session refresh and retry.
			g.log.Info("device gateway returned unauthenticated, refreshing session")
			if _, terr := g.session.Refresh(ctx); terr != nil {
				return nil, terr
			}
			lastErr = err
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func (g *Gateway) handleOnce(ctx context.Context, req *device.Request) (*device.Response, error) {
	// Every gateway call needs the session cookie plus access token.
	if err := g.session.Ensure(ctx); err != nil {
		return nil, err
	}
	payload, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: marshaling request: %v", ErrGateway, err)
	}

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(cctx, http.MethodPost, g.webBase+gatewayPath, bytesReader(encodeGRPCWeb(payload)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/grpc-web+proto")
	httpReq.Header.Set("X-Grpc-Web", "1")
	httpReq.Header.Set("X-User-Agent", "okhttp/4.9.2")
	httpReq.Header.Set("Origin", g.webBase)
	httpReq.Header.Set("Accept", "application/grpc-web+proto")
	g.session.mu.Lock()
	tok := g.session.token
	g.session.mu.Unlock()
	if tok != "" {
		httpReq.Header.Set("Authorization", "Bearer "+tok)
	}
	// The device gateway is a Bearer-only endpoint: cookies must not ride
	// along (the account API rejects cookie+Bearer with 401 token_expired).

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGateway, err)
	}
	defer resp.Body.Close()
	body, err := ioReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: reading response: %v", ErrGateway, err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &grpcStatusError{code: 16, message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
		}
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrGateway, resp.StatusCode, truncateForLog(body))
	}

	data, trailers, err := decodeGRPCWeb(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGateway, err)
	}
	code, msg, err := parseGRPCStatus(trailers)
	if err != nil {
		// Error responses may carry grpc-status as an HTTP response header
		// instead of a trailers frame (empty body, status-only answer).
		if hs := resp.Header.Get("grpc-status"); hs != "" {
			if n, perr := strconv.ParseInt(hs, 10, 32); perr == nil {
				code, msg, err = int32(n), resp.Header.Get("grpc-message"), nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrGateway, err)
		}
	}
	if code != 0 {
		return nil, classifyGRPCStatus(code, msg)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty response payload", ErrGateway)
	}
	var out device.Response
	if err := proto.Unmarshal(data[0], &out); err != nil {
		return nil, fmt.Errorf("%w: unmarshaling response: %v", ErrGateway, err)
	}
	return &out, nil
}

// classifyGRPCStatus turns well-known gateway status codes into app errors.
func classifyGRPCStatus(code int32, msg string) error {
	switch {
	case code == 16:
		return &grpcStatusError{code: code, message: msg}
	case msg != "" && (strings.Contains(msg, "DEVICE_CONNECTION_NOT_FOUND") ||
		strings.Contains(msg, "DEVICE_NOT_CONNECTED")):
		return ErrDeviceOffline
	default:
		return fmt.Errorf("%w (grpc-status %d: %s)", ErrGateway, code, msg)
	}
}

// targetID builds the gateway target for a user terminal or router.
func targetID(kind DeviceKind, id string) string {
	if kind == KindRouter {
		if strings.HasPrefix(id, "Router-") {
			return id
		}
		return "Router-" + id
	}
	if strings.HasPrefix(id, "ut") {
		return id
	}
	return "ut" + id
}

// GetStatus fetches the current status of a dish or router.
func (g *Gateway) GetStatus(ctx context.Context, kind DeviceKind, id string) (*device.Response, error) {
	req := &device.Request{
		TargetId: proto.String(targetID(kind, id)),
		Request: &device.Request_GetStatus{
			GetStatus: &device.GetStatusRequest{},
		},
	}
	return g.Handle(ctx, req)
}

// GetHistory fetches the rolling history buffer (~15 minutes of samples).
func (g *Gateway) GetHistory(ctx context.Context, kind DeviceKind, id string) (*device.DishGetHistoryResponse, error) {
	req := &device.Request{
		TargetId: proto.String(targetID(kind, id)),
		Request: &device.Request_GetHistory{
			GetHistory: &device.GetHistoryRequest{},
		},
	}
	resp, err := g.Handle(ctx, req)
	if err != nil {
		return nil, err
	}
	hist := resp.GetDishGetHistory()
	if hist == nil {
		return nil, fmt.Errorf("%w: no history in response", ErrGateway)
	}
	return hist, nil
}

// Reboot requests a device reboot (serialized per device).
func (g *Gateway) Reboot(ctx context.Context, kind DeviceKind, id string) error {
	return g.mutate(ctx, kind, id, func() *device.Request {
		return &device.Request{
			TargetId: proto.String(targetID(kind, id)),
			Request:  &device.Request_Reboot{Reboot: &device.RebootRequest{}},
		}
	})
}

// Stow or unstows a dish (unstow=false stows it).
func (g *Gateway) Stow(ctx context.Context, id string, unstow bool) error {
	return g.mutate(ctx, KindDish, id, func() *device.Request {
		return &device.Request{
			TargetId: proto.String(targetID(KindDish, id)),
			Request:  &device.Request_DishStow{DishStow: &device.DishStowRequest{Unstow: proto.Bool(unstow)}},
		}
	})
}

// SetPowerSave configures power save mode on a dish. startMinutes is the
// local time of day in minutes when power save starts, durationMinutes how
// long it lasts; enable turns the schedule on or off.
func (g *Gateway) SetPowerSave(ctx context.Context, id string, startMinutes, durationMinutes uint32, enable bool) error {
	return g.mutate(ctx, KindDish, id, func() *device.Request {
		return &device.Request{
			TargetId: proto.String(targetID(KindDish, id)),
			Request: &device.Request_DishPowerSave{DishPowerSave: &device.DishPowerSaveRequest{
				PowerSaveStartMinutes:    proto.Uint32(startMinutes),
				PowerSaveDurationMinutes: proto.Uint32(durationMinutes),
				EnablePowerSave:          proto.Bool(enable),
			}},
		}
	})
}

// InhibitRF pauses or resumes RF transmission on a dish.
func (g *Gateway) InhibitRF(ctx context.Context, id string, inhibit bool) error {
	return g.mutate(ctx, KindDish, id, func() *device.Request {
		return &device.Request{
			TargetId: proto.String(targetID(KindDish, id)),
			Request:  &device.Request_DishInhibitRf{DishInhibitRf: &device.DishInhibitRfRequest{InhibitRf: proto.Bool(inhibit)}},
		}
	})
}

// WifiGetClients returns the WiFi clients connected to a router.
func (g *Gateway) WifiGetClients(ctx context.Context, routerID string) ([]*device.WifiClient, error) {
	req := &device.Request{
		TargetId: proto.String(targetID(KindRouter, routerID)),
		Request: &device.Request_WifiGetClients{
			WifiGetClients: &device.WifiGetClientsRequest{},
		},
	}
	resp, err := g.Handle(ctx, req)
	if err != nil {
		return nil, err
	}
	out := resp.GetWifiGetClients()
	if out == nil {
		return nil, fmt.Errorf("%w: no wifi clients in response", ErrGateway)
	}
	return out.GetClients(), nil
}

// mutate sends a mutation request while holding the per-target lock.
func (g *Gateway) mutate(ctx context.Context, kind DeviceKind, id string, build func() *device.Request) error {
	lock := g.mutationLock(targetID(kind, id))
	lock.Lock()
	defer lock.Unlock()
	req := build()
	_, err := g.Handle(ctx, req)
	return err
}
