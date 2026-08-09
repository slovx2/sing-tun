package tun

import (
	"net/netip"
	"testing"

	"github.com/sagernet/sing-tun/gtcpip/header"
	"github.com/stretchr/testify/require"
)

func TestTCPSessionAcceptHandoffLifecycle(t *testing.T) {
	session := &TCPSession{
		Source:      netip.MustParseAddrPort("10.0.0.2:41000"),
		Destination: netip.MustParseAddrPort("198.18.0.1:443"),
	}

	// 应用发出 SYN 时，内核 listener 尚未完成握手，不能交接。
	require.False(t, session.observeForward(header.TCPFlagSyn))

	// 只有看到内核返回的 SYN-ACK 后，应用 ACK 才拥有交接资格。
	session.observeReverse(header.TCPFlagSyn | header.TCPFlagAck)
	require.True(t, session.observeForward(header.TCPFlagAck))

	// Accept 尚未发生时，重传 ACK 应再次请求交接，避免一次调度让步丢失。
	require.True(t, session.observeForward(header.TCPFlagAck))

	// acceptLoop 接管连接后，数据阶段 ACK 不得再触发调度交接。
	session.markAccepted()
	require.False(t, session.observeForward(header.TCPFlagAck))
}

func TestTCPSessionDoesNotHandoffOrdinaryACK(t *testing.T) {
	session := &TCPSession{}

	// 未观察到本地 listener 的 SYN-ACK，普通 ACK 不能被误判为握手完成。
	require.False(t, session.observeForward(header.TCPFlagAck))
	session.observeReverse(header.TCPFlagAck)
	require.False(t, session.observeForward(header.TCPFlagAck|header.TCPFlagPsh))
}
