package config

// Shell is an optional capability of a Worker, using its existing OS account.
type Shell struct {
	Enabled        bool   `json:"enabled"`
	Executable     string `json:"executable,omitempty"`
	Kind           string `json:"kind,omitempty"`
	MaxRunning     int    `json:"max_running,omitempty"`
	MaxTimeoutMS   int    `json:"max_timeout_ms,omitempty"`
	MaxOutputBytes int64  `json:"max_output_bytes,omitempty"`
}
