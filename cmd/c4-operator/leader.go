package main

import (
	ctrl "sigs.k8s.io/controller-runtime"
)

// managerLease is the Lease, in the operator's namespace, that one manager
// holds at a time.
const managerLease = "c4-manager"

// electLeader makes the manager take the lease before it starts what only
// one manager may run: the work queue and the farm book's sealer, the
// controllers, the capacity actuators, and any runnable that does not say
// otherwise. What says NeedLeaderElection false runs in every manager: the
// webhooks, the workload issuer, and the HUD (its collector and its
// listener, which carries the people's issuer). Without it every manager
// ran everything, and a rolling update (which starts the new pod before it
// stops the old) ran two queues at once.
//
// A manager that stops cleanly gives the lease up once what needs it has
// stopped (or its 30 s grace period is over, and the process exits), so
// the next takes it on its next try (every 2 s, jittered: 4 s in a
// rolling update on a local farm) rather than when it expires (15 s).
// That is safe only because the process ends when the manager does:
// runManager returns mgr.Start's result.
func electLeader(o ctrl.Options, namespace string) ctrl.Options {
	o.LeaderElection = true
	o.LeaderElectionID = managerLease
	o.LeaderElectionNamespace = namespace
	o.LeaderElectionReleaseOnCancel = true
	return o
}
