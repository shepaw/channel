package services

import "testing"

func TestGenerateEndpointSimpleFollowsBaseURL(t *testing.T) {
	const id = "ch-1"

	tests := []struct {
		channelType string
		baseURL     string
		baseDomain  string
		want        string
	}{
		{"http", "https://channel.example.com", "channel.example.com", "https://channel.example.com/proxy/ch-1"},
		{"https", "https://channel.example.com", "channel.example.com", "https://channel.example.com/proxy/ch-1"},
		{"tunnel-http", "https://channel.example.com", "channel.example.com", "https://channel.example.com/proxy/ch-1"},
		{"ws", "https://channel.example.com", "channel.example.com", "wss://channel.example.com/proxy/ch-1"},
		{"tunnel-ws", "https://channel.example.com", "channel.example.com", "wss://channel.example.com/proxy/ch-1"},
		{"http", "http://localhost:8080", "localhost:8080", "http://localhost:8080/proxy/ch-1"},
		{"https", "http://localhost:8080", "localhost:8080", "http://localhost:8080/proxy/ch-1"},
		{"ws", "http://localhost:8080", "localhost:8080", "ws://localhost:8080/proxy/ch-1"},
		{"tunnel-ws", "http://localhost:8080", "localhost:8080", "ws://localhost:8080/proxy/ch-1"},
		{"tcp", "https://channel.example.com", "channel.example.com", "tcp://channel.example.com/channel/ch-1"},
		{"udp", "https://channel.example.com", "channel.example.com", "udp://channel.example.com/channel/ch-1"},
		{"tunnel-tcp", "https://channel.example.com", "channel.example.com", "tcp://channel.example.com/channel/ch-1"},
	}

	for _, tt := range tests {
		got := generateEndpointSimple(tt.channelType, tt.baseURL, tt.baseDomain, id)
		if got != tt.want {
			t.Errorf("%s + %s: got %q, want %q", tt.channelType, tt.baseURL, got, tt.want)
		}
	}
}
