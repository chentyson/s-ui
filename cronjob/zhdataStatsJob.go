package cronjob

import (
	"github.com/alireza0/s-ui/service"
)

// ZhdataStatsJob samples every zhdata external process' per-user traffic
// (GET /v1/stats) and credits the deltas into clients.up/down, the same merge
// semantics the core session tracker flush uses (docs/zhdata-admin-api.md §2).
// 15s mirrors the contract's refresh cadence; the panel traffic column is then
// core traffic + zhdata traffic.
type ZhdataStatsJob struct {
}

func NewZhdataStatsJob() *ZhdataStatsJob {
	return new(ZhdataStatsJob)
}

func (s *ZhdataStatsJob) Run() {
	service.CollectZhdataStats()
}

// Push runs once after a deplete pass: disabled clients must leave the zhdata
// user set right away, so their new connections get rejected by the zhdata
// process rather than served until the next save.
func PushZhdataUsers() {
	service.PushZhdataUsersLatest()
}

// PushZhdataRelay keeps the transparent-relay state of every zhdata process
// aligned with the panel (contract §6); same hook points as the user push.
func PushZhdataRelay() {
	service.PushZhdataRelayLatest()
}
