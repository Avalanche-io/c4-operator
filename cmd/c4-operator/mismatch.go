package main

import (
	"fmt"

	"github.com/Avalanche-io/c4"
	"github.com/go-logr/logr"

	"github.com/Avalanche-io/c4-operator/internal/metrics"
	"github.com/Avalanche-io/c4-operator/internal/view"
)

// watchMismatches counts and logs every read whose bytes are not the C4 ID
// asked for (view.ReadObject), naming both IDs: a store that serves them
// has lost or swapped the object, and needs looking at.
func watchMismatches(log logr.Logger) {
	view.OnMismatch = func(shard string, asked, got c4.ID, refiled error) {
		result := "refiled"
		if refiled != nil {
			result = "left"
		}
		metrics.StoreMismatch.WithLabelValues(shard, result).Inc()
		log.Info("a store served bytes that are not the C4 ID asked for: not present to its reader", "shard", shard, "asked", asked.String(), "got", got.String(), "refile", fmt.Sprint(refiled))
	}
}
