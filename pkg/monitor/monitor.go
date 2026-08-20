// Package monitor defines the future HTTP and Prometheus monitoring surface.
package monitor

const (
	RouteHealth  = "/healthz"
	RouteReady   = "/readyz"
	RouteStatus  = "/api/v1/status"
	RouteClients = "/api/v1/clients"
	RouteStream  = "/api/v1/stream"
	RouteEvents  = "/api/v1/events"
	RouteMetrics = "/metrics"
)
