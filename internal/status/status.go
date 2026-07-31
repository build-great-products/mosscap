// Package status defines stable, secret-safe gateway status DTOs.
package status

// Counts is the current task and deduplication record inventory.
type Counts struct {
	Open           int64 `json:"open"`
	Working        int64 `json:"working"`
	Done           int64 `json:"done"`
	RequestRecords int64 `json:"request_records"`
}

// Snapshot is the stable gateway_status result. It contains identities and
// metrics only; it deliberately never contains task text or secrets.
type Snapshot struct {
	UptimeSeconds     int64  `json:"uptime_seconds"`
	ServerVersion     string `json:"server_version"`
	ProtocolVersion   int    `json:"protocol_version"`
	SchemaVersion     int    `json:"schema_version"`
	PID               int    `json:"pid"`
	Database          string `json:"database"`
	Endpoint          string `json:"endpoint"`
	TaskCounts        Counts `json:"task_counts"`
	DatabaseBytes     int64  `json:"database_bytes"`
	WALBytes          int64  `json:"wal_bytes"`
	Connections       int    `json:"connections"`
	Health            string `json:"health"`
	EffectsCapability string `json:"effects_capability"`
	SlackCapability   string `json:"slack_capability"`
	PiCapability      string `json:"pi_capability"`
}
