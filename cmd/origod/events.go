// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"

	"github.com/latere-ai/origo/internal/events"
)

// newEvents builds the push event dispatcher of spec 008 from the
// configuration and registers its loops. With ORIGO_EVENTS_URL unset the
// dispatcher is off: Enqueue and Emit do nothing and Run only waits.
// The live set is nil until spec 005 wires its membership, so every
// other node's journals count as never heard, which is the single-node
// case the Design names.
func (n *node) newEvents() error {
	d, err := events.New(events.Options{
		Log: n.log, Node: n.cfg.NodeName, URL: n.cfg.EventsURL, Secret: n.cfg.EventsSecret,
		Client:         &http.Client{Transport: outboundTransport(), Timeout: events.DeliveryTimeout},
		RepairInterval: n.cfg.RepairInterval, RepairUnheard: n.cfg.RepairUnheard,
		Metrics: n.reg, Logger: n.logger, Failpoint: n.failpoint,
	})
	if err != nil {
		return err
	}
	n.events = d
	n.background = append(n.background, d.Run)
	return nil
}
