package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/caligone/openqiara/internal/fbxbus"
	"github.com/caligone/openqiara/internal/hlevents"
)

// serveEventCollector takes hl_event_collectd's place on fbxbus: hlcamd
// calls it with its detections (new_notification, iv_event) and the IR-cut
// switches (new_event, flip_flop). The vendor's daemon queued them for the
// dead cloud; dnsmasq sent its POSTs back to openqiarad (/notifications),
// still served for now.
//
// The vendor's daemon must stop first: hlcamd talks to it directly (p2p)
// as long as it lives, even once the name is ours.
func serveEventCollector(ctx context.Context, d *hlevents.Dispatcher, logger *slog.Logger) {
	if out, err := exec.Command("fbxupstartctl", "stop", "hl-event-collectd").CombinedOutput(); err != nil {
		logger.Warn("fbxbus: stopping hl_event_collectd failed", "error", err, "output", strings.TrimSpace(string(out)))
	}
	// The handlers must not block the bus: the dispatcher publishes to
	// MQTT, so it runs on its own goroutine.
	notifs := make(chan hlevents.NotificationItem, 64)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-notifs:
				d.HandleNotification(ctx, n)
			}
		}
	}()
	svc := &fbxbus.Service{
		Name: "hl_event_collectd",
		Log:  logger,
		Methods: map[string]fbxbus.Handler{
			"new_notification": func(arg string) {
				var n hlevents.Notification
				if err := json.Unmarshal([]byte(arg), &n); err != nil {
					logger.Warn("fbxbus: bad notification", "error", err)
					return
				}
				select {
				case notifs <- hlevents.NotificationItem{Timestamp: time.Now().Unix(), Notif: n}:
				default:
					logger.Warn("fbxbus: notification dropped, dispatcher behind")
				}
			},
			"new_event": func(arg string) {
				logger.Debug("fbxbus: hlcamd event", "event", arg) // {"flip_flop": 1}: IR-cut switch
			},
			"flush_queue": func(string) {},
		},
	}
	svc.Run(ctx)
}
