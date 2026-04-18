package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// HwStatic is the stable hardware profile of a host — CPU model, RAM total,
// OS/kernel strings, list of mounted filesystems. Bosun sends this on startup
// and on change (SHA256 of the struct).
type HwStatic struct {
	CPUModel      string    `json:"cpu_model,omitempty"`
	CPUCores      int       `json:"cpu_cores,omitempty"`
	MemTotalBytes uint64    `json:"mem_total_bytes,omitempty"`
	OSPretty      string    `json:"os_pretty,omitempty"`
	KernelVersion string    `json:"kernel_version,omitempty"`
	Mounts        []HwMount `json:"mounts,omitempty"`
}

// HwMount is a filesystem entry from /etc/mtab.
type HwMount struct {
	Mountpoint string `json:"mountpoint"`
	Fstype     string `json:"fstype"`
	Device     string `json:"device,omitempty"`
}

// HwLive is the per-heartbeat snapshot. CPUUsedPct is pre-computed by
// bosun as 100 * cpu_load_1 / cpu_cores so the UI can render the CPU
// chip without first fetching the static block. Disks[] is only populated
// when fastfetch is available on the host and reports disk usage.
type HwLive struct {
	CPULoad1      float64  `json:"cpu_load_1"`
	CPULoad5      float64  `json:"cpu_load_5"`
	CPULoad15     float64  `json:"cpu_load_15"`
	CPUUsedPct    float64  `json:"cpu_used_pct,omitempty"`
	MemTotalBytes uint64   `json:"mem_total_bytes,omitempty"`
	MemUsedBytes  uint64   `json:"mem_used_bytes,omitempty"`
	MemUsedPct    float64  `json:"mem_used_pct,omitempty"`
	CPUTempC      *float64 `json:"cpu_temp_c,omitempty"`
	GPUTempC      *float64 `json:"gpu_temp_c,omitempty"`
	Disks         []HwDisk `json:"disks,omitempty"`
}

// HwDisk is a per-mount usage sample, typically derived from fastfetch output.
type HwDisk struct {
	Mountpoint string  `json:"mountpoint"`
	Fstype     string  `json:"fstype,omitempty"`
	TotalBytes uint64  `json:"total_bytes,omitempty"`
	UsedBytes  uint64  `json:"used_bytes,omitempty"`
	UsedPct    float64 `json:"used_pct,omitempty"`
}

// HostHardware is the aggregate shape returned by GET /api/hosts/{name}/hardware.
type HostHardware struct {
	Hostname    string            `json:"hostname"`
	Static      *HwStatic         `json:"static,omitempty"`
	Live        *HwLive           `json:"live,omitempty"`
	LiveAt      string            `json:"live_at,omitempty"`
	Fastfetch   json.RawMessage   `json:"fastfetch,omitempty"`
	FastfetchAt string            `json:"fastfetch_at,omitempty"`
	Metrics     []HostMetricPoint `json:"metrics"`
}

// HostMetricPoint is a single timeseries row for sparkline rendering.
// CPUTempC / GPUTempC are pointers so null-at-capture renders as a gap.
type HostMetricPoint struct {
	TS         string   `json:"ts"`
	CPULoad    float64  `json:"cpu_load"`
	MemUsedPct float64  `json:"mem_used_pct"`
	CPUTempC   *float64 `json:"cpu_temp_c,omitempty"`
	GPUTempC   *float64 `json:"gpu_temp_c,omitempty"`
}

// insertHostMetric records one sample in the rolling 24h window.
// Must run inside the heartbeat transaction.
func insertHostMetric(tx *sql.Tx, hostID int64, ts string, live *HwLive) error {
	if live == nil {
		return nil
	}
	var cpuTemp, gpuTemp sql.NullFloat64
	if live.CPUTempC != nil {
		cpuTemp = sql.NullFloat64{Float64: *live.CPUTempC, Valid: true}
	}
	if live.GPUTempC != nil {
		gpuTemp = sql.NullFloat64{Float64: *live.GPUTempC, Valid: true}
	}
	if _, err := tx.Exec(
		`INSERT INTO host_metrics (host_id, ts, cpu_load, mem_used_pct, cpu_temp_c, gpu_temp_c) VALUES (?, ?, ?, ?, ?, ?)`,
		hostID, ts, live.CPULoad1, live.MemUsedPct, cpuTemp, gpuTemp,
	); err != nil {
		return fmt.Errorf("insert host_metric: %w", err)
	}
	return nil
}

// PruneOldHostMetrics deletes samples older than the retention window.
func (s *Store) PruneOldHostMetrics(retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention).Format(time.RFC3339)
	res, err := s.DB.Exec(`DELETE FROM host_metrics WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge host_metrics: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// HostHardware returns the full hardware payload for one host.
// Missing sections are returned as nil / empty (not an error).
func (s *Store) HostHardware(hostname string) (*HostHardware, error) {
	var (
		hostID                     int64
		staticBlob, liveBlob       string
		liveAt                     string
		fastfetchBlob, fastfetchAt string
	)
	err := s.DB.QueryRow(
		`SELECT id, hw_static, hw_live, hw_live_at, fastfetch_json, fastfetch_at FROM hosts WHERE hostname = ?`,
		hostname,
	).Scan(&hostID, &staticBlob, &liveBlob, &liveAt, &fastfetchBlob, &fastfetchAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query host: %w", err)
	}

	out := &HostHardware{
		Hostname:    hostname,
		LiveAt:      liveAt,
		FastfetchAt: fastfetchAt,
		Metrics:     []HostMetricPoint{},
	}
	if staticBlob != "" {
		var hs HwStatic
		if err := json.Unmarshal([]byte(staticBlob), &hs); err == nil {
			out.Static = &hs
		}
	}
	if liveBlob != "" {
		var hl HwLive
		if err := json.Unmarshal([]byte(liveBlob), &hl); err == nil {
			out.Live = &hl
		}
	}
	if fastfetchBlob != "" {
		out.Fastfetch = json.RawMessage(fastfetchBlob)
	}

	rows, err := s.DB.Query(
		`SELECT ts, cpu_load, mem_used_pct, cpu_temp_c, gpu_temp_c
		 FROM host_metrics WHERE host_id = ? ORDER BY ts ASC`,
		hostID,
	)
	if err != nil {
		return nil, fmt.Errorf("query metrics: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p HostMetricPoint
		var cpuTemp, gpuTemp sql.NullFloat64
		if err := rows.Scan(&p.TS, &p.CPULoad, &p.MemUsedPct, &cpuTemp, &gpuTemp); err != nil {
			return nil, fmt.Errorf("scan metric: %w", err)
		}
		if cpuTemp.Valid {
			v := cpuTemp.Float64
			p.CPUTempC = &v
		}
		if gpuTemp.Valid {
			v := gpuTemp.Float64
			p.GPUTempC = &v
		}
		out.Metrics = append(out.Metrics, p)
	}
	return out, rows.Err()
}
