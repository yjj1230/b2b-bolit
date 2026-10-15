package wecom

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// MockGateway 客户群出站（0.1 守门员 A3：确认后发送；测试断言发送时机）。
func TestMockGatewaySendGroupCard(t *testing.T) {
	gw := NewMockGateway()
	msgID, err := gw.SendGroupCard(t.Context(), "chang-an", "group-demo", Card{
		TenantID: "chang-an", OrderID: "ORD-1", Kind: "GATEKEEPER_SEND",
	})
	require.NoError(t, err)
	require.Equal(t, "mock-group-msg-1", msgID)

	cards := gw.SentGroupCards()
	require.Len(t, cards, 1)
	require.Equal(t, "chang-an", cards[0].TenantID)
	require.Equal(t, "group-demo", cards[0].GroupID)
	require.Equal(t, "ORD-1", cards[0].Card.OrderID)

	// 未发送前为空
	gw2 := NewMockGateway()
	require.Empty(t, gw2.SentGroupCards())
}
