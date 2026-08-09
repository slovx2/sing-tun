//go:build linux

package tun

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/netlink"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const systemIntegrationEnv = "SING_TUN_SYSTEM_INTEGRATION"

// TestSystemStackConcurrentConnectRetry exercises the production system-stack
// path with a real Linux TUN and kernel TCP listener. Clients use ordinary
// connection deadlines and bounded retries, matching application behavior
// under a burst of cold-start connections.
func TestSystemStackConcurrentConnectRetry(t *testing.T) {
	if os.Getenv(systemIntegrationEnv) != "1" {
		t.Skip("requires a Linux network namespace with CAP_NET_ADMIN and /dev/net/tun")
	}

	const (
		clientCount = 48
		maxAttempts = 3
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	options := Options{
		Name:                      "sbtun0",
		Inet4Address:              []netip.Prefix{netip.MustParsePrefix("172.31.0.1/30")},
		MTU:                       1500,
		DNSMode:                   DNSModeDisabled,
		EXP_ExternalConfiguration: true,
	}
	tunDevice, err := New(options)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tunDevice.Close() })
	require.NoError(t, configureSystemIntegrationTUN(options.Name))
	require.NoError(t, tunDevice.Start())

	handler := &systemIntegrationHandler{}
	stack, err := NewSystem(StackOptions{
		Context:     ctx,
		Tun:         tunDevice,
		TunOptions:  options,
		UDPTimeout:  time.Minute,
		ICMPTimeout: time.Minute,
		Handler:     handler,
		Logger:      logger.NOP(),
	})
	require.NoError(t, err)
	require.NoError(t, stack.Start())
	t.Cleanup(func() { _ = stack.Close() })

	start := make(chan struct{})
	var retries atomic.Int32
	var failures atomic.Int32
	var clients sync.WaitGroup
	clients.Add(clientCount)
	for index := range clientCount {
		go func() {
			defer clients.Done()
			<-start
			destination := netip.AddrFrom4([4]byte{198, 18, byte(index / 250), byte(index%250 + 1)})
			for attempt := range maxAttempts {
				if attempt > 0 {
					retries.Add(1)
					time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
				}
				if systemIntegrationRoundTrip(destination.String()+":443") == nil {
					return
				}
			}
			failures.Add(1)
		}()
	}
	close(start)
	clients.Wait()

	require.Zero(t, failures.Load(), "connections still failed after bounded retries")
	require.Zero(t, retries.Load(), "healthy local system-stack handshakes should not time out")
	require.EqualValues(t, clientCount, handler.accepted.Load())
}

func configureSystemIntegrationTUN(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	address, err := netlink.ParseAddr("172.31.0.1/30")
	if err != nil {
		return err
	}
	if err = netlink.AddrAdd(link, address); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	if err = netlink.LinkSetUp(link); err != nil {
		return err
	}
	destination, err := netlink.ParseIPNet("198.18.0.0/15")
	if err != nil {
		return err
	}
	return netlink.RouteAdd(&netlink.Route{Dst: destination, LinkIndex: link.Attrs().Index})
}

func systemIntegrationRoundTrip(address string) error {
	dialer := net.Dialer{Timeout: 750 * time.Millisecond}
	conn, err := dialer.Dial("tcp4", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(750 * time.Millisecond)); err != nil {
		return err
	}
	if _, err = conn.Write([]byte("ping")); err != nil {
		return err
	}
	response := make([]byte, 4)
	if _, err = io.ReadFull(conn, response); err != nil {
		return err
	}
	if string(response) != "pong" {
		return errors.New("unexpected response")
	}
	return nil
}

type systemIntegrationHandler struct {
	accepted atomic.Int32
}

func (h *systemIntegrationHandler) JudgeFlow(uint8, netip.AddrPort, netip.AddrPort, []byte) FlowVerdict {
	return FlowVerdict{Action: ActionAccept}
}

func (h *systemIntegrationHandler) NewConnectionEx(_ context.Context, conn net.Conn, _, _ M.Socksaddr, _ N.CloseHandlerFunc) {
	h.accepted.Add(1)
	defer conn.Close()
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil || string(request) != "ping" {
		return
	}
	_, _ = conn.Write([]byte("pong"))
}

func (h *systemIntegrationHandler) NewDNSPacket([]byte, M.Socksaddr, M.Socksaddr, N.PacketWriter) {
}

func (h *systemIntegrationHandler) NewPacketConnectionEx(context.Context, N.PacketConn, M.Socksaddr, M.Socksaddr, N.CloseHandlerFunc) {
}

var _ Handler = (*systemIntegrationHandler)(nil)
