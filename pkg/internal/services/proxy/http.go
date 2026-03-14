package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/edenzou/channel-service/pkg/internal/services"
)

type HTTPProxy struct {
	proxy *httputil.ReverseProxy
}

func NewHTTPProxy(target string) (*HTTPProxy, error) {
	targetURL, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set("X-Channel-Proxy", "true")
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, fmt.Sprintf("Proxy error: %v", err), http.StatusBadGateway)
	}

	return &HTTPProxy{proxy: proxy}, nil
}

func (h *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request, channelID, clientIP string, rateLimitSvc *services.RateLimitService, channelSvc *services.ChannelService) {
	// Check request rate limit
	allowed, _, err := rateLimitSvc.CheckRequests(channelID, clientIP)
	if err != nil || !allowed {
		http.Error(w, "Too many requests", http.StatusTooManyRequests)
		return
	}

	// Buffer body to measure size
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}

	// Check bandwidth limit
	allowed, _, err = rateLimitSvc.CheckBandwidth(channelID, clientIP, int64(len(bodyBytes)))
	if err != nil || !allowed {
		http.Error(w, "Bandwidth limit exceeded", http.StatusTooManyRequests)
		return
	}

	// Wrap response writer to capture response size
	cw := &captureWriter{ResponseWriter: w, buf: &bytes.Buffer{}}
	h.proxy.ServeHTTP(cw, r)

	totalBytes := int64(len(bodyBytes)) + int64(cw.buf.Len())
	go channelSvc.UpdateChannelStats(channelID, totalBytes, 1, 0)
}

type captureWriter struct {
	http.ResponseWriter
	buf        *bytes.Buffer
	statusCode int
}

func (c *captureWriter) WriteHeader(code int) {
	c.statusCode = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}