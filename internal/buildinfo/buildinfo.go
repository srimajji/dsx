package buildinfo

import "runtime"

var (
	Version      = "dev"
	Commit       = "unknown"
	BuiltAt      = "unknown"
	GuestSHA256  = "unknown"
	AgentImage   = "unknown"
	BrowserImage = "unknown"
)

type Info struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	BuiltAt      string `json:"built_at"`
	GoVersion    string `json:"go_version"`
	GuestSHA256  string `json:"guest_sha256"`
	AgentImage   string `json:"agent_image"`
	BrowserImage string `json:"browser_image"`
}

func Current() Info {
	return Info{
		Version:      Version,
		Commit:       Commit,
		BuiltAt:      BuiltAt,
		GoVersion:    runtime.Version(),
		GuestSHA256:  GuestSHA256,
		AgentImage:   AgentImage,
		BrowserImage: BrowserImage,
	}
}
