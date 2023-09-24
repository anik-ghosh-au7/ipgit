package models

import "time"

type Commit struct {
	CID       string            `json:"cid"`
	ParentCID string            `json:"parent_cid"`
	Message   string            `json:"message"`
	Timestamp time.Time         `json:"timestamp"`
	Files     map[string]string `json:"files"`
}
