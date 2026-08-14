package services

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInboxSubscriberConn_wantsTarget(t *testing.T) {
	all := &InboxSubscriberConn{CallerFP: "abcd1234abcd1234"}
	require.True(t, all.wantsTarget("agent_a"))

	filtered := &InboxSubscriberConn{
		CallerFP: "abcd1234abcd1234",
		targets:  map[string]struct{}{"agent_a": {}},
	}
	require.True(t, filtered.wantsTarget("agent_a"))
	require.False(t, filtered.wantsTarget("agent_b"))
}

func TestInboxSubscriberManager_RegisterUnregister(t *testing.T) {
	mgr := NewInboxSubscriberManager()
	c := &InboxSubscriberConn{CallerFP: "abcd1234abcd1234"}
	mgr.Register("abcd1234abcd1234", c)
	mgr.Unregister("abcd1234abcd1234", c)
	// should not panic when notifying after unregister
	mgr.NotifyReply("abcd1234abcd1234", InboxReplyNotification{Type: "mail_reply"})
}
