package main

const (
	typeCheckpoint = "checkpoint"
	typeDefault    = "default"
)

type containerStartupInfo struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	Type      string `json:"type"`
}
