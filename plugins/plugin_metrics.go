package plugins

import (
	figo "github.com/bi0dread/figo/v4"
)

import (
	"sync"
	"time"
)

// Performance monitoring is provided as a plugin rather than core figo state:
// create a MetricsPlugin (or a bare PerformanceMonitor) and attach it to the
// collaborators that produce metrics — e.g. CachePlugin.SetPerformanceMonitor —
// or record into it manually via RecordQuery.

// Metrics represents performance metrics
type Metrics struct {
	QueryCount     int64
	CacheHits      int64
	CacheMisses    int64
	AverageLatency time.Duration
	TotalLatency   time.Duration
	ErrorCount     int64
	LastQueryTime  time.Time
}

// PerformanceMonitor tracks query performance
type PerformanceMonitor struct {
	metrics *Metrics
	enabled bool
	mu      sync.RWMutex
}

// NewPerformanceMonitor creates a new performance monitor
func NewPerformanceMonitor(enabled bool) *PerformanceMonitor {
	return &PerformanceMonitor{
		metrics: &Metrics{},
		enabled: enabled,
	}
}

// RecordQuery records a query execution
func (pm *PerformanceMonitor) RecordQuery(latency time.Duration, cacheHit bool, err error) {
	if !pm.enabled {
		return
	}

	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.metrics.QueryCount++
	pm.metrics.TotalLatency += latency
	pm.metrics.AverageLatency = time.Duration(int64(pm.metrics.TotalLatency) / pm.metrics.QueryCount)
	pm.metrics.LastQueryTime = time.Now()

	if cacheHit {
		pm.metrics.CacheHits++
	} else {
		pm.metrics.CacheMisses++
	}

	if err != nil {
		pm.metrics.ErrorCount++
	}
}

// GetMetrics returns current metrics
func (pm *PerformanceMonitor) GetMetrics() Metrics {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// Create a copy to avoid returning a value that contains a lock
	return Metrics{
		QueryCount:     pm.metrics.QueryCount,
		CacheHits:      pm.metrics.CacheHits,
		CacheMisses:    pm.metrics.CacheMisses,
		AverageLatency: pm.metrics.AverageLatency,
		TotalLatency:   pm.metrics.TotalLatency,
		ErrorCount:     pm.metrics.ErrorCount,
		LastQueryTime:  pm.metrics.LastQueryTime,
	}
}

// Reset resets all metrics
func (pm *PerformanceMonitor) Reset() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pm.metrics = &Metrics{}
}

// MetricsPlugin wraps a PerformanceMonitor as a registerable plugin. It embeds
// the monitor, so RecordQuery/GetMetrics/Reset are available directly on the
// plugin, and the embedded PerformanceMonitor can be handed to collaborators
// that record metrics (e.g. CachePlugin.SetPerformanceMonitor).
type MetricsPlugin struct {
	*PerformanceMonitor
}

// NewMetricsPlugin creates a metrics plugin with its own monitor
func NewMetricsPlugin(enabled bool) *MetricsPlugin {
	return &MetricsPlugin{PerformanceMonitor: NewPerformanceMonitor(enabled)}
}

// Name implements Plugin
func (p *MetricsPlugin) Name() string { return "figo-metrics" }

// Version implements Plugin
func (p *MetricsPlugin) Version() string { return "1.0.0" }

// Initialize implements Plugin
func (p *MetricsPlugin) Initialize(figo.Figo) error { return nil }

// BeforeQuery implements Plugin
func (p *MetricsPlugin) BeforeQuery(figo.Figo, any) error { return nil }

// AfterQuery implements Plugin
func (p *MetricsPlugin) AfterQuery(figo.Figo, any, interface{}) error { return nil }

// BeforeParse implements Plugin
func (p *MetricsPlugin) BeforeParse(_ figo.Figo, dsl string) (string, error) { return dsl, nil }

// AfterParse implements Plugin
func (p *MetricsPlugin) AfterParse(figo.Figo, string) error { return nil }
