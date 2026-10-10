package hlevents

import (
	"encoding/json"
	"fmt"
)

// ParseNotification parse le JSON d'un appel new_notification de hlcamd
// sur fbxbus : {"type":"iv_event","data":{...}}.
func ParseNotification(arg string) (Notification, error) {
	var n Notification
	if err := json.Unmarshal([]byte(arg), &n); err != nil {
		return n, fmt.Errorf("parse notification: %w", err)
	}
	return n, nil
}
