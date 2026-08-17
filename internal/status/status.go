// Package status defines stable, secret-safe gateway status DTOs.
package status

// Counts is the current task and deduplication record inventory.
type Counts struct {
	Open           int64 `json:"open"`
	Working        int64 `json:"working"`
	Done           int64 `json:"done"`
	RequestRecords int64 `json:"request_records"`
}

// PiDeliveryStatus is bounded gateway-owned delivery state. It deliberately
// excludes task bodies, diagnostics, paths, and session internals.
// FileTransferStatus is payload-free lifecycle inventory. It intentionally
// excludes names, paths, hashes, URLs, and file contents.
type FileTransferStatus struct {
	Capability             string           `json:"capability"`
	TemporaryCount         int64            `json:"temporary_count"`
	TemporaryBytes         int64            `json:"temporary_bytes"`
	NearestExpiry          *string          `json:"nearest_expiry,omitempty"`
	IncomingByState        map[string]int64 `json:"incoming_by_state"`
	OutgoingByState        map[string]int64 `json:"outgoing_by_state"`
	UnsafeTemporaryEntries int64            `json:"unsafe_temporary_entries"`
}

type PiDeliveryStatus struct {
	ActivePiSessionID string `json:"active_pi_session_id,omitempty"`
	Pending           int64  `json:"pending"`
	Inserting         int64  `json:"inserting"`
	Persisted         int64  `json:"persisted"`
	Uncertain         int64  `json:"uncertain"`
	Rejected          int64  `json:"rejected"`
}

// Snapshot is the stable gateway_status result. It contains identities and
// metrics only; it deliberately never contains task text or secrets.
type Snapshot struct {
	UptimeSeconds          int64              `json:"uptime_seconds"`
	ServerVersion          string             `json:"server_version"`
	ProtocolVersion        int                `json:"protocol_version"`
	SchemaVersion          int                `json:"schema_version"`
	ActiveConfigGeneration int64              `json:"active_config_generation"`
	ActiveConfigDigest     string             `json:"active_config_digest,omitempty"`
	PID                    int                `json:"pid"`
	Database               string             `json:"database"`
	Endpoint               string             `json:"endpoint"`
	TaskCounts             Counts             `json:"task_counts"`
	DatabaseBytes          int64              `json:"database_bytes"`
	WALBytes               int64              `json:"wal_bytes"`
	Connections            int                `json:"connections"`
	Health                 string             `json:"health"`
	EffectsCapability      string             `json:"effects_capability"`
	SlackCapability        string             `json:"slack_capability"`
	PiCapability           string             `json:"pi_capability"`
	PiDelivery             PiDeliveryStatus   `json:"pi_delivery"`
	FileTransfer           FileTransferStatus `json:"file_transfer"`
	// Slack is the current operational state. It is populated by server without
	// carrying tokens, headers, message bodies, or file bytes.
	Slack any `json:"slack,omitempty"`
}
