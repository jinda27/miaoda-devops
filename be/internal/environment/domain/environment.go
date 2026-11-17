package domain

import "time"

const (
	Ready           = "ready"
	Missing         = "missing"
	VersionMismatch = "version_mismatch"
)

type Item struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Requirement      string `json:"requirement"`
	Version          string `json:"version,omitempty"`
	Path             string `json:"path,omitempty"`
	Status           string `json:"status"`
	Message          string `json:"message,omitempty"`
	PackageAvailable bool   `json:"packageAvailable"`
}

type Step struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
}

type Installation struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	Percent    int       `json:"percent"`
	Steps      []Step    `json:"steps"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Message    string    `json:"message,omitempty"`
}

type Report struct {
	Platform     string        `json:"platform"`
	Architecture string        `json:"architecture"`
	Supported    bool          `json:"supported"`
	Ready        bool          `json:"ready"`
	Root         string        `json:"root"`
	Packages     string        `json:"packages"`
	DocumentURL  string        `json:"documentURL"`
	Items        []Item        `json:"items"`
	Installation *Installation `json:"installation,omitempty"`
}
